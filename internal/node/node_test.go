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
	if err := os.WriteFile(filepath.Join(cfg.ShareDir, name+".txt"), []byte("share of "+name), 0o644); err != nil {
		t.Fatal(err)
	}
	tn := &tnode{t: t, name: name, cfg: cfg}
	tn.start()
	t.Cleanup(tn.stop)
	return tn
}

func (tn *tnode) start() {
	tn.t.Helper()
	// CERCLE_REAL_GOSSIP=1: real 30 s interval, to check the real delays.
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
		tn.t.Errorf("%s: Run: %v", tn.name, err)
	}
	tn.n.Close()
	tn.cancel = nil
}

func (tn *tnode) id() string { return tn.n.PublicKey().String() }

func (tn *tnode) self() membership.Member {
	m, _ := tn.n.Membership().Self()
	return m
}

// eventually waits until cond holds, d at most.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > d {
			t.Fatalf("%v timeout exceeded: %s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

// info queries a peer through from's tunnel.
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

	// Criterion: a new node creates a circle in one click and gets 10.42.0.1.
	if err := alice.n.CreateCircle(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := alice.self().TunnelIP; ip != netip.MustParseAddr("10.42.0.1") {
		t.Fatalf("founder at %v", ip)
	}

	// Criterion: a second node joins with a pasted code, no other configuration.
	code, _, err := alice.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.n.Join(ctx, code); err != nil {
		t.Fatalf("bob joins: %v", err)
	}
	eventually(t, 10*time.Second, "bob reaches alice", func() bool { _, err := info(bob, alice.self()); return err == nil })
	eventually(t, 10*time.Second, "alice reaches bob", func() bool { _, err := info(alice, bob.self()); return err == nil })

	// Criterion: a used code is refused with a clear message.
	if err := carol.n.Join(ctx, code); !errors.Is(err, membership.ErrInviteUsed) {
		t.Fatalf("reused code: %v", err)
	}

	// Criterion: a third node invited by the second shows up at the first in
	// under 2 minutes, and both talk directly.
	code2, _, err := bob.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := carol.n.Join(ctx, code2); err != nil {
		t.Fatalf("carol joins: %v", err)
	}
	d := eventually(t, 2*time.Minute, "alice knows carol", func() bool { return knows(alice, carol.id()) })
	d2 := eventually(t, 2*time.Minute, "alice and carol talk", func() bool {
		_, e1 := info(alice, carol.self())
		_, e2 := info(carol, alice.self())
		return e1 == nil && e2 == nil
	})
	t.Logf("carol known to alice in %v, direct link %v later", d.Round(time.Millisecond), d2.Round(time.Millisecond))

	// The three addresses are distinct and inside the subnet.
	seen := map[netip.Addr]string{}
	for _, tn := range []*tnode{alice, bob, carol} {
		ip := tn.self().TunnelIP
		if other, dup := seen[ip]; dup || !tn.cfg.TunnelSubnet.Contains(ip) {
			t.Fatalf("address %v of %s (duplicate of %q?)", ip, tn.name, other)
		}
		seen[ip] = tn.name
	}

	// Criterion: a revoked member loses access to every node in under 2 minutes.
	carolSelf := carol.self()
	if err := alice.n.Revoke(ctx, carol.id()); err != nil {
		t.Fatal(err)
	}
	d = eventually(t, 2*time.Minute, "carol cut off from alice and bob", func() bool {
		_, e1 := info(carol, alice.self())
		_, e2 := info(carol, bob.self())
		return e1 != nil && e2 != nil
	})
	t.Logf("carol cut off from everyone in %v", d.Round(time.Millisecond))
	if _, err := info(alice, carolSelf); err == nil {
		t.Error("alice still reaches carol")
	}

	// Including after a restart: alice restarts, carol stays out, bob stays in.
	alice.stop()
	alice.start()
	if m, ok := alice.n.Membership().Member(carol.id()); !ok || !m.Revoked {
		t.Fatalf("revocation lost on restart: %+v", m)
	}
	eventually(t, 10*time.Second, "bob reaches restarted alice", func() bool { _, err := info(bob, alice.self()); return err == nil })
	time.Sleep(time.Second)
	if _, err := info(carol, alice.self()); err == nil {
		t.Error("carol reaches alice after her restart")
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
		t.Fatalf("expired code: %v", err)
	}
	if err := bob.n.Join(ctx, "cercle1:nimportequoi"); !errors.Is(err, membership.ErrBadCode) {
		t.Fatalf("unreadable code: %v", err)
	}
	if err := alice.n.Join(ctx, code); !errors.Is(err, membership.ErrAlreadyInCircle) && !errors.Is(err, membership.ErrInviteExpired) {
		t.Fatalf("member joining: %v", err)
	}
	if bob.n.TunnelUp() {
		t.Error("bob's tunnel started despite the failure")
	}
}

// TestDownloadBetweenMembers: the download queue goes through the circle.
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
	eventually(t, 10*time.Second, "link", func() bool { _, err := info(bob, alice.self()); return err == nil })
	if _, _, err := bob.n.Queue().Enqueue(ctx, alice.id(), []string{""}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bob.cfg.DownloadsDir, "alice", "alice.txt")
	eventually(t, 20*time.Second, "file received", func() bool {
		b, err := os.ReadFile(path)
		return err == nil && string(b) == "share of alice"
	})
}

// TestEnqueueWhileRecordChanges: regression test for a deadlock found on
// 2026-11-05. Queuing kept a write transaction open during the network call to
// the peer; a concurrent record update, holding the circle lock and waiting
// for the write connection, then blocked the whole node.
func TestEnqueueWhileRecordChanges(t *testing.T) {
	ctx := context.Background()
	alice, bob := newTNode(t, "alice"), newTNode(t, "bob")
	for i := range 3000 {
		p := filepath.Join(alice.cfg.ShareDir, "batch", fmt.Sprintf("f%04d.txt", i))
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
	eventually(t, 10*time.Second, "link", func() bool { _, err := info(bob, alice.self()); return err == nil })

	stop := make(chan struct{})
	churn := make(chan struct{})
	go func() { // keeps changing bob's record
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
			if _, _, err := bob.n.Queue().Enqueue(ctx, alice.id(), []string{"batch"}); err != nil {
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
		t.Fatal("deadlock: queuing does not finish during record updates")
	}
	close(stop)
	<-churn
	sum, err := bob.n.Queue().Summarize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := sum.Count["queued"] + sum.Count["active"] + sum.Count["done"] + sum.Count["waiting"]; n != 3000 {
		t.Errorf("%d items queued, want 3000 (no duplicates)", n)
	}
}

// circleOfTwo founds a circle at alice and brings bob in.
func circleOfTwo(t *testing.T) (alice, bob *tnode) {
	t.Helper()
	ctx := context.Background()
	alice, bob = newTNode(t, "alice"), newTNode(t, "bob")
	if err := alice.n.CreateCircle(ctx); err != nil {
		t.Fatal(err)
	}
	code, _, err := alice.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.n.Join(ctx, code); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "bob reaches alice", func() bool { _, err := info(bob, alice.self()); return err == nil })
	return alice, bob
}

// freeAddr returns an address of the circle no member uses.
func freeAddr(t *testing.T, nodes ...*tnode) netip.Addr {
	t.Helper()
	for ip := netip.MustParseAddr("10.42.0.77"); ; ip = ip.Next() {
		taken := false
		for _, tn := range nodes {
			taken = taken || tn.self().TunnelIP == ip
		}
		if !taken {
			return ip
		}
	}
}

// TestInviteAddressTaken: a code whose address another member took in the
// meantime is refused with a clear message, instead of cutting off the new
// member.
func TestInviteAddressTaken(t *testing.T) {
	ctx := context.Background()
	alice, bob := circleOfTwo(t)
	carol, dave := newTNode(t, "carol"), newTNode(t, "dave")
	ip := freeAddr(t, alice, bob)
	alice.n.Membership().SetInviteAddress(ip)
	bob.n.Membership().SetInviteAddress(ip)
	codeA, _, err := alice.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	codeB, _, err := bob.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := carol.n.Join(ctx, codeA); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "bob knows carol", func() bool { return knows(bob, carol.id()) })
	if err := dave.n.Join(ctx, codeB); !errors.Is(err, membership.ErrInviteAddressTaken) {
		t.Fatalf("join with a taken address: %v", err)
	}
	if dave.n.TunnelUp() {
		t.Error("dave's tunnel started despite the refusal")
	}
	code, _, err := bob.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := dave.n.Join(ctx, code); err != nil {
		t.Fatalf("join with a new code: %v", err)
	}
	eventually(t, 30*time.Second, "dave reaches carol", func() bool { _, err := info(dave, carol.self()); return err == nil })
}

// TestSimultaneousAddressConflict: two members invite at the same time and
// both codes carry the same address (the end-to-end failure of 2026-11-06).
// The younger member must move to a free address and everyone must reach
// everyone.
func TestSimultaneousAddressConflict(t *testing.T) {
	ctx := context.Background()
	alice, bob := circleOfTwo(t)
	carol, dave := newTNode(t, "carol"), newTNode(t, "dave")
	ip := freeAddr(t, alice, bob)
	alice.n.Membership().SetInviteAddress(ip)
	bob.n.Membership().SetInviteAddress(ip)
	codeA, _, err := alice.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	codeB, _, err := bob.n.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// No gossip between alice and bob: neither learns the other's new member
	// before both are admitted.
	alice.n.noGossip.Store(true)
	bob.n.noGossip.Store(true)
	if err := carol.n.Join(ctx, codeA); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // carol is the older one
	if err := dave.n.Join(ctx, codeB); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // each inviter routes its new member
	alice.n.noGossip.Store(false)
	bob.n.noGossip.Store(false)
	alice.n.poke()
	bob.n.poke()

	all := []*tnode{alice, bob, carol, dave}
	d := eventually(t, 90*time.Second, "everyone reaches everyone", func() bool {
		for _, a := range all {
			for _, b := range all {
				if a == b {
					continue
				}
				if _, err := info(a, b.self()); err != nil {
					return false
				}
			}
		}
		return true
	})
	t.Logf("conflict settled in %v", d.Round(time.Millisecond))
	if carol.self().TunnelIP != ip || dave.self().TunnelIP == ip {
		t.Errorf("carol at %v, dave at %v: the older member must keep %v", carol.self().TunnelIP, dave.self().TunnelIP, ip)
	}
	for _, tn := range all {
		for _, m := range tn.n.Membership().Members() {
			if m.Conflict {
				t.Errorf("%s still sees a conflict on %s", tn.name, m.Name)
			}
		}
	}
}
