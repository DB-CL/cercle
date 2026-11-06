package wg

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "node.key")
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %v, want 0600", fi.Mode().Perm())
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil || k1 != k2 {
		t.Fatalf("reloaded key differs: %v", err)
	}
	if k1.Public() == k1 || k1.Public().IsZero() {
		t.Fatal("inconsistent public key")
	}
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("an unreadable key must be reported, not replaced")
	}
}

func TestKeyText(t *testing.T) {
	k, _ := GenerateKey()
	b, _ := k.MarshalText()
	var back Key
	if err := back.UnmarshalText(b); err != nil || back != k {
		t.Fatalf("base64 round trip failed: %v", err)
	}
	if _, err := ParseKey("dHJvcCBjb3VydA=="); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestParseIpcGet(t *testing.T) {
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	raw := "private_key=00\nlisten_port=51820\n" +
		"public_key=" + k1.Hex() + "\nendpoint=203.0.113.4:51820\nlast_handshake_time_sec=1700000000\nlast_handshake_time_nsec=5\n" +
		"tx_bytes=10\nrx_bytes=20\nallowed_ip=10.42.0.2/32\n" +
		"public_key=" + k2.Hex() + "\nlast_handshake_time_sec=0\nlast_handshake_time_nsec=0\nallowed_ip=10.42.0.3/32\n"
	peers, err := parseIpcGet(raw)
	if err != nil || len(peers) != 2 {
		t.Fatalf("parseIpcGet = %v, %v", peers, err)
	}
	for _, p := range peers {
		switch p.PublicKey {
		case k1:
			if p.Endpoint.String() != "203.0.113.4:51820" || p.RxBytes != 20 || p.TxBytes != 10 ||
				!p.LastHandshake.Equal(time.Unix(1700000000, 5)) || p.AllowedIPs[0].String() != "10.42.0.2/32" {
				t.Errorf("peer 1 misread: %+v", p)
			}
		case k2:
			if !p.LastHandshake.IsZero() || p.Endpoint.IsValid() {
				t.Errorf("peer 2 misread: %+v", p)
			}
		}
	}
}

// node is a test node listening on UDP 127.0.0.1.
type node struct {
	key  Key
	dev  *Device
	port int
	ip   netip.Addr
}

func newNode(t testing.TB, ip string) *node {
	t.Helper()
	return newNodeOpts(t, ip, false)
}

func newNodeOpts(t testing.TB, ip string, upstream bool) *node {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(Options{PrivateKey: k, Address: netip.MustParseAddr(ip), upstreamNetstack: upstream})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	port, err := d.ListenPort()
	if err != nil {
		t.Fatal(err)
	}
	return &node{key: k, dev: d, port: port, ip: netip.MustParseAddr(ip)}
}

func (n *node) asPeer() Peer {
	return Peer{PublicKey: n.key.Public(), TunnelIP: n.ip, Endpoint: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(n.port))}
}

// link connects two nodes both ways.
func link(t testing.TB, a, b *node) {
	t.Helper()
	if err := a.dev.SetPeers([]Peer{b.asPeer()}); err != nil {
		t.Fatal(err)
	}
	if err := b.dev.SetPeers([]Peer{a.asPeer()}); err != nil {
		t.Fatal(err)
	}
}

func echoServer(t testing.TB, n *node, port int) {
	t.Helper()
	l, err := n.dev.Listen(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
}

func dial(ctx context.Context, n *node, to netip.Addr, port int) (net.Conn, error) {
	return n.dev.DialContext(ctx, "tcp", netip.AddrPortFrom(to, uint16(port)).String())
}

func TestTunnelEchoAndReconcile(t *testing.T) {
	a := newNode(t, "10.42.0.1")
	b := newNode(t, "10.42.0.2")
	link(t, a, b)
	echoServer(t, a, 7)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dial(ctx, b, a.ip, 7)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello through the tunnel")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != string(msg) {
		t.Fatalf("echo = %q, %v", got, err)
	}
	c.Close()

	st, err := b.dev.Peers()
	if err != nil || len(st) != 1 || !st[0].Online(time.Now()) || st[0].RxBytes == 0 {
		t.Fatalf("peer state after exchange: %+v, %v", st, err)
	}

	// Reconciling with a wrong endpoint must not break the live session.
	wrong := a.asPeer()
	wrong.Endpoint = netip.MustParseAddrPort("127.0.0.1:9")
	if err := b.dev.SetPeers([]Peer{wrong}); err != nil {
		t.Fatal(err)
	}
	st, _ = b.dev.Peers()
	if st[0].Endpoint.Port() != uint16(a.port) {
		t.Fatalf("live endpoint overwritten: %v", st[0].Endpoint)
	}

	// Peer removed: no connection possible any more.
	if err := b.dev.SetPeers(nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := b.dev.Peers(); len(st) != 0 {
		t.Fatalf("peer not removed: %+v", st)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if c, err := dial(ctx2, b, a.ip, 7); err == nil {
		c.Close()
		t.Fatal("connection possible after removing the peer")
	}
}

func TestSetPeersRejectsInvalid(t *testing.T) {
	a := newNode(t, "10.42.0.1")
	b := newNode(t, "10.42.0.2")
	p := b.asPeer()
	if err := a.dev.SetPeers([]Peer{p, p}); err == nil {
		t.Error("duplicate peer accepted")
	}
	p.TunnelIP = netip.Addr{}
	if err := a.dev.SetPeers([]Peer{p}); err == nil {
		t.Error("peer without address accepted")
	}
}
