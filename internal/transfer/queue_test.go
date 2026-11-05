package transfer_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/peerapi"
	"github.com/DB-CL/cercle/internal/store"
	"github.com/DB-CL/cercle/internal/transfer"
)

const peerID = "YWxpY2UtaWQ="

// source est un pair « alice » servi en HTTP ordinaire, avec journal des
// requêtes de fichier et coupure programmable.
type source struct {
	t       *testing.T
	share   string
	ix      *index.Index
	uploads *transfer.Uploads
	srv     *httptest.Server

	mu     sync.Mutex
	ranges []string // en-tête Range de chaque requête /file, dans l'ordre
	paths  []string
	cutAt  atomic.Int64 // > 0 : la prochaine réponse est coupée après ce nombre d'octets
}

func newSource(t *testing.T, files map[string][]byte) *source {
	t.Helper()
	base := t.TempDir()
	s := &source{t: t, share: filepath.Join(base, "share")}
	for rel, b := range files {
		s.put(rel, b)
	}
	st, err := store.Open(context.Background(), filepath.Join(base, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s.ix = index.New(st, s.share, nil, nil)
	s.rescan()
	s.uploads = transfer.NewUploads(0, 3, nil)
	api := &peerapi.Server{
		CircleID:  "c",
		Directory: dir{netip.MustParseAddr("127.0.0.1"): {Name: "bob"}},
		Info:      func(context.Context) peerapi.Info { return peerapi.Info{} },
		Index:     s.ix,
		Uploads:   s.uploads,
	}
	h := api.Handler()
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/file" {
			s.mu.Lock()
			s.ranges = append(s.ranges, r.Header.Get("Range"))
			s.paths = append(s.paths, r.URL.Query().Get("path"))
			s.mu.Unlock()
			if n := s.cutAt.Swap(0); n > 0 {
				w = &cutWriter{ResponseWriter: w, left: n}
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *source) put(rel string, b []byte) {
	s.t.Helper()
	p := filepath.Join(s.share, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *source) rescan() {
	s.t.Helper()
	if _, err := s.ix.Scan(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

func (s *source) requests() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...), append([]string(nil), s.paths...)
}

// cutWriter coupe la connexion après left octets de corps.
type cutWriter struct {
	http.ResponseWriter
	left int64
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		n, _ := c.ResponseWriter.Write(p[:c.left])
		c.ResponseWriter.(http.Flusher).Flush()
		c.left = 0
		_ = n
		panic(http.ErrAbortHandler)
	}
	c.left -= int64(len(p))
	return c.ResponseWriter.Write(p)
}

type dir map[netip.Addr]peerapi.Member

func (d dir) MemberByIP(ip netip.Addr) (peerapi.Member, bool) { m, ok := d[ip]; return m, ok }

// sink est le nœud « bob » qui télécharge.
type sink struct {
	t    *testing.T
	st   *store.Store
	dl   string
	m    *transfer.Manager
	stop context.CancelFunc
	done chan struct{}
}

func newSink(t *testing.T, src *source, dbPath, dl string) *sink {
	t.Helper()
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "dst.db")
	}
	if dl == "" {
		dl = t.TempDir()
	}
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	addr := src.srv.Listener.Addr().String()
	client := peerapi.NewClient(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}, "c")
	m := transfer.NewManager(transfer.Options{
		Store:  st,
		Remote: client,
		Peers: func(id string) (transfer.Peer, bool) {
			return transfer.Peer{ID: peerID, Name: "alice", IP: netip.MustParseAddr("127.0.0.1")}, id == peerID
		},
		Dir:         dl,
		Concurrency: 2,
		BackoffBase: 50 * time.Millisecond,
		BackoffMax:  200 * time.Millisecond,
	})
	return &sink{t: t, st: st, dl: dl, m: m}
}

func (s *sink) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stop, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		if err := s.m.Run(ctx); err != nil {
			s.t.Errorf("Run : %v", err)
		}
	}()
	s.t.Cleanup(s.shutdown)
}

func (s *sink) shutdown() {
	if s.stop != nil {
		s.stop()
		<-s.done
		s.stop = nil
	}
}

func (s *sink) enqueue(paths ...string) int {
	s.t.Helper()
	n, _, err := s.m.Enqueue(context.Background(), peerID, paths)
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

// wait attend que cond soit vraie sur la file, 10 s au plus.
func (s *sink) wait(what string, cond func([]transfer.Item) bool) []transfer.Item {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		items, err := s.m.List(context.Background())
		if err != nil {
			s.t.Fatal(err)
		}
		if cond(items) {
			return items
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("délai dépassé en attendant : %s ; file : %+v", what, items)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func all(st transfer.Status) func([]transfer.Item) bool {
	return func(items []transfer.Item) bool {
		for _, it := range items {
			if it.Status != st {
				return false
			}
		}
		return len(items) > 0
	}
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (s *sink) check(src *source, rel string) {
	s.t.Helper()
	got, err := os.ReadFile(filepath.Join(s.dl, "alice", rel))
	if err != nil {
		s.t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(src.share, rel))
	if sha256.Sum256(got) != sha256.Sum256(want) {
		s.t.Fatalf("%s : contenu différent (%d octets contre %d)", rel, len(got), len(want))
	}
	gi, _ := os.Stat(filepath.Join(s.dl, "alice", rel))
	wi, _ := os.Stat(filepath.Join(src.share, rel))
	if !gi.ModTime().Equal(wi.ModTime()) {
		s.t.Errorf("%s : date %v, attendu %v", rel, gi.ModTime(), wi.ModTime())
	}
	if parts, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Join(s.dl, "alice", rel)), ".cercle-*.part")); len(parts) > 0 {
		s.t.Errorf("fichiers partiels restés sur le disque : %v", parts)
	}
}

func TestDownloadFolderAndSkipExisting(t *testing.T) {
	src := newSource(t, map[string][]byte{
		"films/a.mkv": random(3 << 20), "films/série/e01.mkv": random(1000), "films/série/e02.mkv": random(10),
		"films/vide.txt": {}, "autre.txt": random(5),
	})
	dst := newSink(t, src, "", "")
	if n := dst.enqueue("films"); n != 4 {
		t.Fatalf("%d éléments ajoutés, attendu 4", n)
	}
	dst.start()
	dst.wait("tout terminé", all(transfer.Done))
	for _, rel := range []string{"films/a.mkv", "films/série/e01.mkv", "films/série/e02.mkv", "films/vide.txt"} {
		dst.check(src, rel)
	}
	if _, err := os.Stat(filepath.Join(dst.dl, "alice", "autre.txt")); err == nil {
		t.Error("autre.txt téléchargé sans avoir été demandé")
	}

	// Remise en file : rien de nouveau, aucune requête de fichier.
	_, before := src.requests()
	if n := dst.enqueue("films"); n != 0 {
		t.Errorf("remise en file : %d éléments ajoutés, attendu 0", n)
	}
	time.Sleep(100 * time.Millisecond)
	if _, after := src.requests(); len(after) != len(before) {
		t.Errorf("fichiers déjà présents retéléchargés : %v", after[len(before):])
	}

	// Un fichier déjà présent avec la même taille, inconnu de la file : terminé sans transfert.
	dst2 := newSink(t, src, "", dst.dl)
	if n := dst2.enqueue("films/a.mkv"); n != 1 {
		t.Fatalf("ajout : %d", n)
	}
	items, _ := dst2.m.List(context.Background())
	if items[0].Status != transfer.Done {
		t.Errorf("fichier présent : statut %s, attendu done", items[0].Status)
	}
}

func TestResumeAfterCut(t *testing.T) {
	src := newSource(t, map[string][]byte{"gros.bin": random(4 << 20)})
	src.cutAt.Store(1 << 20)
	dst := newSink(t, src, "", "")
	dst.enqueue("gros.bin")
	dst.start()
	items := dst.wait("terminé", all(transfer.Done))
	dst.check(src, "gros.bin")
	ranges, _ := src.requests()
	if len(ranges) != 2 || ranges[0] != "" || !strings.HasPrefix(ranges[1], "bytes=") {
		t.Fatalf("requêtes : %q ; attendu une complète puis une reprise", ranges)
	}
	off, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(ranges[1], "bytes="), "-"))
	if off <= 0 || off > 1<<20 {
		t.Fatalf("reprise à l'octet %d, attendu entre 1 et %d", off, 1<<20)
	}
	if items[0].Attempts != 1 {
		t.Errorf("tentatives = %d, attendu 1", items[0].Attempts)
	}
}

func TestChangedSourceRestarts(t *testing.T) {
	src := newSource(t, map[string][]byte{"f.bin": random(100000)})
	dst := newSink(t, src, "", "")
	dst.enqueue("f.bin")
	// Un .part de l'ancienne version, puis le fichier change chez alice.
	must(t, os.MkdirAll(filepath.Join(dst.dl, "alice"), 0o755))
	items, _ := dst.m.List(context.Background())
	must(t, os.WriteFile(filepath.Join(dst.dl, "alice", fmt.Sprintf(".cercle-%d.part", items[0].ID)), random(5000), 0o644))
	time.Sleep(10 * time.Millisecond)
	src.put("f.bin", random(120000))

	dst.start()
	items = dst.wait("terminé", all(transfer.Done))
	dst.check(src, "f.bin")
	if items[0].Size != 120000 {
		t.Errorf("taille en file = %d, attendu la nouvelle (120000)", items[0].Size)
	}
	ranges, _ := src.requests()
	if len(ranges) != 1 || ranges[0] != "bytes=5000-" {
		t.Errorf("requêtes : %q", ranges)
	}
}

func TestRestartKeepsQueue(t *testing.T) {
	src := newSource(t, map[string][]byte{"lent.bin": random(3 << 20)})
	src.uploads.SetLimit(1 << 20)
	db := filepath.Join(t.TempDir(), "dst.db")
	dl := t.TempDir()

	dst := newSink(t, src, db, dl)
	dst.enqueue("lent.bin")
	dst.start()
	dst.wait("transfert entamé", func(items []transfer.Item) bool {
		return items[0].Status == transfer.Active && items[0].BytesDone > 512<<10
	})
	dst.shutdown() // arrêt du conteneur en plein transfert
	dst.st.Close()

	src.uploads.SetLimit(0)
	dst2 := newSink(t, src, db, dl)
	items, _ := dst2.m.List(context.Background())
	if len(items) != 1 || items[0].Status != transfer.Queued || items[0].BytesDone <= 0 {
		t.Fatalf("file après redémarrage : %+v", items)
	}
	dst2.start()
	dst2.wait("terminé", all(transfer.Done))
	dst2.check(src, "lent.bin")
	ranges, _ := src.requests()
	if len(ranges) != 2 || !strings.HasPrefix(ranges[1], "bytes=") || ranges[1] == "bytes=0-" {
		t.Fatalf("requêtes : %q ; attendu une reprise après le redémarrage", ranges)
	}
}

func TestErrorsAndBusyPeer(t *testing.T) {
	src := newSource(t, map[string][]byte{"disparu.bin": random(10), "occupé.bin": random(10)})
	dst := newSink(t, src, "", "")
	dst.enqueue("disparu.bin")
	must(t, os.Remove(filepath.Join(src.share, "disparu.bin"))) // encore dans l'index d'alice
	dst.start()
	items := dst.wait("erreur définitive", all(transfer.Failed))
	if items[0].LastError == "" {
		t.Error("erreur sans message")
	}

	// Relance après le retour du fichier.
	src.put("disparu.bin", random(10))
	must(t, dst.m.Retry(context.Background(), items[0].ID))
	dst.wait("relance terminée", all(transfer.Done))

	// Créneau d'upload fermé chez alice : 503, attente sans compter d'échec.
	w, _ := config.ParseWindow(time.Now().Add(2*time.Hour).Format("15:04") + "-" + time.Now().Add(3*time.Hour).Format("15:04"))
	src.uploads.SetWindow(&w)
	dst.enqueue("occupé.bin")
	items = dst.wait("en attente", func(items []transfer.Item) bool {
		for _, it := range items {
			if it.RemotePath == "occupé.bin" && it.Status == transfer.Waiting {
				return true
			}
		}
		return false
	})
	for _, it := range items {
		if it.RemotePath == "occupé.bin" {
			if it.Attempts != 0 || time.Until(it.NextAttempt) < time.Hour {
				t.Errorf("503 : tentatives %d, prochain essai dans %v ; attendu 0 et environ 2 h", it.Attempts, time.Until(it.NextAttempt).Round(time.Minute))
			}
		}
	}
}

func TestPriorityPauseRemove(t *testing.T) {
	src := newSource(t, map[string][]byte{"a.bin": random(2 << 20), "b.bin": random(10), "c.bin": random(10)})
	src.uploads.SetLimit(1 << 20)
	dst := newSink(t, src, "", "")
	dst.enqueue("a.bin", "b.bin", "c.bin")
	items, _ := dst.m.List(context.Background())
	ids := map[string]int64{}
	for _, it := range items {
		ids[it.RemotePath] = it.ID
	}
	ctx := context.Background()
	must(t, dst.m.Prioritize(ctx, ids["c.bin"]))
	dst.start()
	// Un seul transfert à la fois par pair : c (prioritaire), puis a, puis b.
	dst.wait("a en cours", func(items []transfer.Item) bool {
		return items[0].RemotePath == "a.bin" && items[0].Status == transfer.Active && items[0].BytesDone > 0
	})
	_, paths := src.requests()
	if paths[0] != "c.bin" {
		t.Errorf("ordre des requêtes : %v ; c.bin prioritaire attendu en premier", paths)
	}
	must(t, dst.m.Pause(ctx, ids["a.bin"]))
	dst.wait("a suspendu, b terminé", func(items []transfer.Item) bool {
		st := map[string]transfer.Status{}
		for _, it := range items {
			st[it.RemotePath] = it.Status
		}
		return st["a.bin"] == transfer.Paused && st["b.bin"] == transfer.Done
	})
	part := filepath.Join(dst.dl, "alice", fmt.Sprintf(".cercle-%d.part", ids["a.bin"]))
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("le .part de a doit rester après une pause : %v", err)
	}
	must(t, dst.m.Remove(ctx, ids["a.bin"], true))
	if _, err := os.Stat(part); err == nil {
		t.Error(".part non supprimé")
	}
	items, _ = dst.m.List(ctx)
	if len(items) != 2 {
		t.Errorf("file après retrait : %+v", items)
	}
}

// TestPartNameCollision : un fichier distant nommé « x.part » ne doit pas
// pouvoir écraser, par le fichier partiel de « x », un fichier déjà reçu.
func TestPartNameCollision(t *testing.T) {
	a, b := random(3000), random(2000)
	src := newSource(t, map[string][]byte{"x.part": a, "x": b})
	dst := newSink(t, src, "", "")
	dst.enqueue("x.part")
	dst.start()
	dst.wait("x.part reçu", all(transfer.Done))
	dst.enqueue("x")
	dst.wait("x reçu", all(transfer.Done))
	dst.check(src, "x.part")
	dst.check(src, "x")
}

func TestMaliciousPeer(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	must(t, err)
	defer st.Close()
	dl := t.TempDir()
	m := transfer.NewManager(transfer.Options{
		Store:  st,
		Remote: liar{},
		Peers: func(id string) (transfer.Peer, bool) {
			return transfer.Peer{ID: id, Name: "../../etc", IP: netip.MustParseAddr("10.42.0.9")}, true
		},
		Dir: dl,
	})
	added, skipped, err := m.Enqueue(context.Background(), "x", []string{"", "../../", "/etc"})
	must(t, err)
	if added != 1 || skipped != 2+2 {
		t.Fatalf("ajoutés %d, refusés %d ; attendu 1 et 4", added, skipped)
	}
	items, _ := m.List(context.Background())
	if items[0].DestPath != "_.._etc/ok.txt" {
		t.Errorf("destination = %q", items[0].DestPath)
	}
}

// liar est un pair qui renvoie des chemins piégés.
type liar struct{}

func (liar) Tree(_ context.Context, _ netip.Addr, _ string, fn func(index.Entry) error) error {
	for _, p := range []string{"../../.ssh/authorized_keys", "/etc/passwd", "ok.txt"} {
		if err := fn(index.Entry{Path: p, Size: 1}); err != nil {
			return err
		}
	}
	return nil
}

func (liar) Download(context.Context, netip.Addr, string, int64, string) (*http.Response, error) {
	return nil, http.ErrServerClosed
}

func TestDownloadLimit(t *testing.T) {
	src := newSource(t, map[string][]byte{"a.bin": random(3 << 20)})
	dst := newSink(t, src, "", "")
	dst.m.SetLimit(2 << 20)
	dst.enqueue("a.bin")
	start := time.Now()
	dst.start()
	dst.wait("terminé", all(transfer.Done))
	if el := time.Since(start); el < 1200*time.Millisecond || el > 2500*time.Millisecond {
		t.Errorf("3 Mio à 2 Mio/s en %v, attendu environ 1,4 s", el)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
