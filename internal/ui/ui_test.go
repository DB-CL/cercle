package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/node"
)

type tnode struct {
	t   *testing.T
	n   *node.Node
	srv *httptest.Server
	cfg config.Config
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func start(t *testing.T, name, password string, files map[string]string) *tnode {
	t.Helper()
	base := t.TempDir()
	cfg, _ := config.Load(func(string) string { return "" })
	port := freeUDPPort(t)
	cfg.NodeName, cfg.WGListenPort = name, port
	cfg.WGPublicEndpoints = []string{fmt.Sprintf("127.0.0.1:%d", port)}
	cfg.DataDir, cfg.ShareDir, cfg.DownloadsDir = filepath.Join(base, "data"), filepath.Join(base, "share"), filepath.Join(base, "dl")
	for rel, content := range files {
		p := filepath.Join(cfg.ShareDir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(cfg.ShareDir, 0o755)
	n, err := node.New(context.Background(), node.Options{Config: cfg, GossipInterval: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	srv := httptest.NewServer((&Server{Node: n, Password: password}).Handler())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
		n.Close()
	})
	return &tnode{t: t, n: n, srv: srv, cfg: cfg}
}

// call fait une requête à l'API de l'interface ; out reçoit le JSON.
func (tn *tnode) call(method, path string, body, out any) int {
	tn.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, tn.srv.URL+"/ui/api"+path, r)
	req.Header.Set(CSRFHeader, "1")
	req.SetBasicAuth("", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tn.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("délai dépassé : %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStaticAndSecurity(t *testing.T) {
	a := start(t, "alice", "secret", nil)
	get := func(path string, auth bool) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, a.srv.URL+path, nil)
		if auth {
			req.SetBasicAuth("x", "secret")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := get("/", false); r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("sans mot de passe : %d", r.StatusCode)
	}
	r := get("/", true)
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), "<title>Cercle</title>") {
		t.Fatalf("page : %d", r.StatusCode)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP absente : %q", csp)
	}
	for _, f := range []string{"/app.js", "/style.css"} {
		if r := get(f, true); r.StatusCode != 200 {
			t.Errorf("%s : %d", f, r.StatusCode)
		}
	}
	if r := get("/healthz", false); r.StatusCode != 200 {
		t.Errorf("healthz : %d", r.StatusCode)
	}

	// CSRF : une modification sans l'en-tête est refusée, même authentifiée.
	req, _ := http.NewRequest(http.MethodPost, a.srv.URL+"/ui/api/circle", nil)
	req.SetBasicAuth("x", "secret")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST sans %s : %d, attendu 403", CSRFHeader, resp.StatusCode)
	}
	if a.n.Membership().InCircle() {
		t.Fatal("cercle créé malgré le refus")
	}
}

func TestFullFlow(t *testing.T) {
	alice := start(t, "alice", "secret", map[string]string{"films/a.mkv": "AAAA", "films/b.mkv": "BB", "lisez-moi.txt": "salut"})
	bob := start(t, "bob", "secret", nil)

	var st statusResp
	if code := alice.call("POST", "/circle", nil, &st); code != 200 || st.Self == nil || st.Self.TunnelIP.String() != "10.42.0.1" {
		t.Fatalf("création : %d %+v", code, st)
	}
	if code := alice.call("POST", "/circle", nil, nil); code != http.StatusConflict {
		t.Errorf("seconde création : %d", code)
	}
	var inv struct {
		Code string `json:"code"`
	}
	if code := alice.call("POST", "/invites", nil, &inv); code != 201 || !strings.HasPrefix(inv.Code, "cercle1:") {
		t.Fatalf("invitation : %d", code)
	}
	if code := bob.call("POST", "/join", map[string]string{"code": inv.Code}, &st); code != 200 || !st.InCircle {
		t.Fatalf("adhésion : %d %+v", code, st)
	}
	var e map[string]string
	if code := bob.call("POST", "/join", map[string]string{"code": inv.Code}, &e); code != http.StatusConflict {
		t.Errorf("seconde adhésion : %d %v", code, e)
	}

	var aliceID string
	eventually(t, "alice en ligne chez bob", func() bool {
		var ms []node.PeerState
		bob.call("GET", "/members", nil, &ms)
		for _, m := range ms {
			if m.Name == "alice" && m.Online {
				aliceID = m.NodeID
				return true
			}
		}
		return false
	})
	if _, err := alice.n.Index().Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	var page struct {
		Entries []struct {
			Path  string `json:"path"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
		} `json:"entries"`
	}
	if code := bob.call("GET", "/members/"+url.PathEscape(aliceID)+"/files?path=", nil, &page); code != 200 || len(page.Entries) != 2 || !page.Entries[0].IsDir || page.Entries[0].Size != 6 {
		t.Fatalf("navigation : %d %+v", code, page)
	}
	var added map[string]int
	if code := bob.call("POST", "/queue", map[string]any{"peer": aliceID, "paths": []string{"films", "lisez-moi.txt"}}, &added); code != 200 || added["added"] != 3 {
		t.Fatalf("mise en file : %d %v", code, added)
	}
	eventually(t, "téléchargements terminés", func() bool {
		var q queueResp
		bob.call("GET", "/queue?status=done", nil, &q)
		return q.Summary.Count["done"] == 3
	})
	got, _ := os.ReadFile(filepath.Join(bob.cfg.DownloadsDir, "alice", "films", "a.mkv"))
	if string(got) != "AAAA" {
		t.Fatalf("fichier reçu : %q", got)
	}
	var cleared map[string]int64
	bob.call("POST", "/queue/actions", map[string]string{"action": "clear_done"}, &cleared)
	if cleared["removed"] != 3 {
		t.Errorf("vidage des terminés : %v", cleared)
	}

	// Réglages : refus champ par champ, puis application à chaud et propagation du nom.
	var bad struct {
		Fields map[string]string `json:"fields"`
	}
	s := bob.n.Settings()
	s.UploadLimit, s.DownloadWindow = "vite", "25:00-01:00"
	if code := bob.call("PUT", "/settings", s, &bad); code != http.StatusUnprocessableEntity || bad.Fields["upload_limit"] == "" || bad.Fields["download_window"] == "" {
		t.Fatalf("réglages invalides : %d %+v", code, bad)
	}
	s = bob.n.Settings()
	s.Name, s.UploadLimit, s.MaxUploads, s.DownloadWindow = "bob-nas", "5M", 2, "01:00-07:00"
	var applied node.Settings
	if code := bob.call("PUT", "/settings", s, &applied); code != 200 || applied.UploadLimit != "5M" || applied.DownloadWindow != "01:00-07:00" {
		t.Fatalf("réglages : %d %+v", code, applied)
	}
	if limit, max, _, _ := bob.n.Uploads().Limits(); limit != 5<<20 || max != 2 {
		t.Errorf("brides non appliquées : %d, %d", limit, max)
	}
	eventually(t, "nouveau nom de bob chez alice", func() bool {
		var ms []node.PeerState
		alice.call("GET", "/members", nil, &ms)
		for _, m := range ms {
			if m.Name == "bob-nas" {
				return true
			}
		}
		return false
	})
}

func TestEventsStream(t *testing.T) {
	a := start(t, "alice", "", nil)
	req, _ := http.NewRequest(http.MethodGet, a.srv.URL+"/ui/api/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	want := []string{"event: hello", "event: settings"}
	triggered := false
	for sc.Scan() {
		line := sc.Text()
		if line == want[0] {
			want = want[1:]
			if len(want) == 0 {
				return
			}
			if !triggered {
				triggered = true
				go func() {
					s := a.n.Settings()
					s.Name = "alice2"
					_ = a.n.SetSettings(context.Background(), s)
				}()
			}
		}
	}
	t.Fatalf("événements manquants : %v (%v)", want, sc.Err())
}
