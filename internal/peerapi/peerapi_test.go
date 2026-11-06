package peerapi

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/wg"
)

// staticDir is a fixed directory for tests.
type staticDir map[netip.Addr]Member

func (d staticDir) MemberByIP(ip netip.Addr) (Member, bool) {
	m, ok := d[ip]
	return m, ok
}

type testNode struct {
	name string
	key  wg.Key
	dev  *wg.Device
	ip   netip.Addr
	port int
}

func startNode(t *testing.T, name, ip string) *testNode {
	t.Helper()
	k, err := wg.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	d, err := wg.New(wg.Options{PrivateKey: k, Address: netip.MustParseAddr(ip)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	port, err := d.ListenPort()
	if err != nil {
		t.Fatal(err)
	}
	return &testNode{name: name, key: k, dev: d, ip: netip.MustParseAddr(ip), port: port}
}

func (n *testNode) peer() wg.Peer {
	return wg.Peer{PublicKey: n.key.Public(), TunnelIP: n.ip, Endpoint: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(n.port))}
}

func (n *testNode) member() Member {
	return Member{NodeID: n.key.Public().String(), Name: n.name, TunnelIP: n.ip}
}

func (n *testNode) serve(t *testing.T, circle string, dir Directory) {
	t.Helper()
	l, err := n.dev.Listen(Port)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		CircleID:  circle,
		Directory: dir,
		Info: func(ctx context.Context) Info {
			m, _ := MemberFrom(ctx)
			return Info{Name: n.name, Version: "test for " + m.Name}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	})
}

// TestInfoThroughTunnel: alice serves the API; bob is a member; carol is a
// valid WireGuard peer of alice but missing from the directory, so refused.
func TestInfoThroughTunnel(t *testing.T) {
	const circle = "test-circle"
	alice := startNode(t, "alice", "10.42.0.1")
	bob := startNode(t, "bob", "10.42.0.2")
	carol := startNode(t, "carol", "10.42.0.3")

	if err := alice.dev.SetPeers([]wg.Peer{bob.peer(), carol.peer()}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{bob, carol} {
		if err := n.dev.SetPeers([]wg.Peer{alice.peer()}); err != nil {
			t.Fatal(err)
		}
	}
	alice.serve(t, circle, staticDir{bob.ip: bob.member()})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := NewClient(bob.dev.DialContext, circle).Info(ctx, alice.ip)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "alice" || info.Version != "test for bob" {
		t.Fatalf("info = %+v; bob's identity did not reach the handler", info)
	}

	var se *StatusError
	_, err = NewClient(carol.dev.DialContext, circle).Info(ctx, alice.ip)
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("carol, missing from the directory: %v; want 403", err)
	}

	_, err = NewClient(bob.dev.DialContext, "other-circle").Info(ctx, alice.ip)
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("wrong circle: %v; want 403", err)
	}
}

func TestUnreachablePeerTimesOut(t *testing.T) {
	bob := startNode(t, "bob", "10.42.0.2")
	ghost := startNode(t, "ghost", "10.42.0.9")
	p := ghost.peer()
	p.Endpoint = netip.MustParseAddrPort("127.0.0.1:9") // nobody listens
	if err := bob.dev.SetPeers([]wg.Peer{p}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := NewClient(bob.dev.DialContext, "c").Info(context.Background(), ghost.ip)
	if err == nil {
		t.Fatal("expected an error")
	}
	if el := time.Since(start); el > 13*time.Second {
		t.Fatalf("connect timeout not honored: %v", el)
	}
}
