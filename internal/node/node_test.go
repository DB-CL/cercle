package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/membership"
	"github.com/DB-CL/cercle/internal/peerapi"
)

type tnode struct {
	t      *testing.T
	name   string
	cfg    config.Config
	n      *Node
	cancel context.CancelFunc
	done   chan error
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func newTNode(t *testing.T, name string) *tnode {
	t.Helper()
	base := t.TempDir()
	cfg, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	port := freeUDPPort(t)
	cfg.NodeName = name
	cfg.WGListenPort = port
	cfg.WGPublicEndpoints = []string{fmt.Sprintf("127.0.0.1:%d", port)}
	cfg.DataDir = filepath.Join(base, "data")
	cfg.ShareDir = filepath.Join(base, "share")
	cfg.DownloadsDir = filepath.Join(base, "downloads")
	if err := os.MkdirAll(cfg.ShareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ShareDir, name+".txt"), []byte("partage de "+name), 0o644); err != nil {
		t.Fatal(err)
	}
	tn := &tnode{t: t, name: name, cfg: cfg}
	tn.start()
	t.Cleanup(tn.stop)
	return tn
}

func (tn *tnode) start() {
	tn.t.Helper()
	// CERCLE_REAL_GOSSIP=1 : intervalle réel de 30 s, pour vérifier les délais du PRD.
	interval := 300 * time.Millisecond
	if os.Getenv("CERCLE_REAL_GOSSIP") != "" {
		interval = 30 * time.Second
	}
	n, err := New(context.Background(), Options{Config: tn.cfg, GossipInterval: interval})
	if err != nil {
		tn.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tn.n, tn.cancel, tn.done = n, cancel, make(chan error, 1)
	go func() { tn.done <- n.Run(ctx) }()
}

func (tn *tnode) stop() {
	if tn.cancel == nil {
		return
	}
	tn.cancel()
	if err := <-tn.done; err != nil {
		tn.t.Errorf("%s : Run : %v", tn.name, err)
	}
	tn.n.Close()
	tn.cancel = nil
}

func (tn *tnode) id() string { return tn.n.PublicKey().String() }

func (tn *tnode) self() membership.Member {
	m, _ := tn.n.Membership().Self()
	return m
}

// eventually attend que cond devienne vraie, au plus d.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > d {
			t.Fatalf("délai de %v dépassé : %s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

// info interroge un pair à travers le tunnel de from.
func info(from *tnode, to membership.Member) (peerapi.Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return from.n.peerClient().Info(ctx, to.TunnelIP)
}

func knows(a *tnode, id string) bool {
	_, ok := a.n.Membership().Member(id)
	return ok
}

func TestCircleAcceptance(t *testing.T) {
	ctx := context.Background()
	alice, bob, carol := newTNode(t, "alice"), newTNode(t, "bob"), newTNode(t, "carol")

	// Critère : un nœud neuf crée un cercle en un clic et obtient 10.42.0.1.
	if err := alice.n.CreateCircle(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := alice.self().TunnelIP; ip != netip.MustParseAddr("10.42.0.1") {
		t.Fatalf("fondateur en %v", ip)
	}

	// Critère : un second nœud rejoint avec un code collé, sans autre configuration.
	code, _, err := alice.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.n.Join(ctx, code); err != nil {
		t.Fatalf("bob rejoint : %v", err)
	}
	eventually(t, 10*time.Second, "bob joint alice", func() bool { _, err := info(bob, alice.self()); return err == nil })
	eventually(t, 10*time.Second, "alice joint bob", func() bool { _, err := info(alice, bob.self()); return err == nil })

	// Critère : un code déjà utilisé est refusé avec un message clair.
	if err := carol.n.Join(ctx, code); !errors.Is(err, membership.ErrInviteUsed) {
		t.Fatalf("code réutilisé : %v", err)
	}

	// Critère : un troisième nœud invité par le deuxième apparaît chez le premier
	// en moins de 2 minutes, et les deux se parlent directement.
	code2, _, err := bob.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := carol.n.Join(ctx, code2); err != nil {
		t.Fatalf("carol rejoint : %v", err)
	}
	d := eventually(t, 2*time.Minute, "alice connaît carol", func() bool { return knows(alice, carol.id()) })
	d2 := eventually(t, 2*time.Minute, "alice et carol se parlent", func() bool {
		_, e1 := info(alice, carol.self())
		_, e2 := info(carol, alice.self())
		return e1 == nil && e2 == nil
	})
	t.Logf("carol connue d'alice en %v, liaison directe en %v de plus", d.Round(time.Millisecond), d2.Round(time.Millisecond))

	// Les trois adresses sont distinctes et dans le sous-réseau.
	seen := map[netip.Addr]string{}
	for _, tn := range []*tnode{alice, bob, carol} {
		ip := tn.self().TunnelIP
		if other, dup := seen[ip]; dup || !tn.cfg.TunnelSubnet.Contains(ip) {
			t.Fatalf("adresse %v de %s (doublon avec %q ?)", ip, tn.name, other)
		}
		seen[ip] = tn.name
	}

	// Critère : un membre révoqué perd l'accès à tous les nœuds en moins de 2 minutes.
	carolSelf := carol.self()
	if err := alice.n.Revoke(ctx, carol.id()); err != nil {
		t.Fatal(err)
	}
	d = eventually(t, 2*time.Minute, "carol coupée d'alice et de bob", func() bool {
		_, e1 := info(carol, alice.self())
		_, e2 := info(carol, bob.self())
		return e1 != nil && e2 != nil
	})
	t.Logf("carol coupée de tous en %v", d.Round(time.Millisecond))
	if _, err := info(alice, carolSelf); err == nil {
		t.Error("alice joint encore carol")
	}

	// Y compris après redémarrage : alice redémarre, carol reste dehors, bob reste dedans.
	alice.stop()
	alice.start()
	if m, ok := alice.n.Membership().Member(carol.id()); !ok || !m.Revoked {
		t.Fatalf("révocation perdue au redémarrage : %+v", m)
	}
	eventually(t, 10*time.Second, "bob joint alice redémarrée", func() bool { _, err := info(bob, alice.self()); return err == nil })
	time.Sleep(time.Second)
	if _, err := info(carol, alice.self()); err == nil {
		t.Error("carol joint alice après son redémarrage")
	}
}

func TestExpiredAndBadCodes(t *testing.T) {
	ctx := context.Background()
	alice, bob := newTNode(t, "alice"), newTNode(t, "bob")
	if err := alice.n.CreateCircle(ctx); err != nil {
		t.Fatal(err)
	}
	alice.n.cfg.InviteTTL = time.Millisecond
	code, _, err := alice.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := bob.n.Join(ctx, code); !errors.Is(err, membership.ErrInviteExpired) {
		t.Fatalf("code expiré : %v", err)
	}
	if err := bob.n.Join(ctx, "cercle1:nimportequoi"); !errors.Is(err, membership.ErrBadCode) {
		t.Fatalf("code illisible : %v", err)
	}
	if err := alice.n.Join(ctx, code); !errors.Is(err, membership.ErrAlreadyInCircle) && !errors.Is(err, membership.ErrInviteExpired) {
		t.Fatalf("membre qui rejoint : %v", err)
	}
	if bob.n.TunnelUp() {
		t.Error("tunnel de bob démarré malgré l'échec")
	}
}

// TestDownloadBetweenMembers : la file de J3 passe par le cercle de J4.
func TestDownloadBetweenMembers(t *testing.T) {
	ctx := context.Background()
	alice, bob := newTNode(t, "alice"), newTNode(t, "bob")
	if err := alice.n.CreateCircle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.n.Index().Scan(ctx); err != nil {
		t.Fatal(err)
	}
	code, _, _ := alice.n.Invite(ctx)
	if err := bob.n.Join(ctx, code); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "liaison", func() bool { _, err := info(bob, alice.self()); return err == nil })
	if _, _, err := bob.n.Queue().Enqueue(ctx, alice.id(), []string{""}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bob.cfg.DownloadsDir, "alice", "alice.txt")
	eventually(t, 20*time.Second, "fichier reçu", func() bool {
		b, err := os.ReadFile(path)
		return err == nil && string(b) == "partage de alice"
	})
}

// TestEnqueueWhileFicheChanges : régression d'un interblocage du 05/11/2026.
// La mise en file gardait une transaction d'écriture ouverte pendant l'appel
// réseau au pair ; une mise à jour de fiche simultanée, qui tient le verrou du
// cercle et attend la connexion d'écriture, bloquait alors tout le nœud.
func TestEnqueueWhileFicheChanges(t *testing.T) {
	ctx := context.Background()
	alice, bob := newTNode(t, "alice"), newTNode(t, "bob")
	for i := range 3000 {
		p := filepath.Join(alice.cfg.ShareDir, "lot", fmt.Sprintf("f%04d.txt", i))
		if i == 0 {
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := alice.n.CreateCircle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.n.Index().Scan(ctx); err != nil {
		t.Fatal(err)
	}
	code, _, _ := alice.n.Invite(ctx)
	if err := bob.n.Join(ctx, code); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "liaison", func() bool { _, err := info(bob, alice.self()); return err == nil })

	stop := make(chan struct{})
	churn := make(chan struct{})
	go func() { // modifie sans arrêt la fiche de bob
		defer close(churn)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = bob.n.Membership().UpdateSelf(ctx, func(b *membership.Body) {
				b.Announced = []string{fmt.Sprintf("127.0.0.1:%d", 40000+i%1000)}
			})
		}
	}()
	done := make(chan error, 1)
	go func() {
		for range 5 {
			if _, _, err := bob.n.Queue().Enqueue(ctx, alice.id(), []string{"lot"}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("interblocage : la mise en file ne se termine pas pendant les mises à jour de fiche")
	}
	close(stop)
	<-churn
	sum, err := bob.n.Queue().Summarize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := sum.Count["queued"] + sum.Count["active"] + sum.Count["done"] + sum.Count["waiting"]; n != 3000 {
		t.Errorf("%d éléments en file, attendu 3000 (sans doublon)", n)
	}
}
