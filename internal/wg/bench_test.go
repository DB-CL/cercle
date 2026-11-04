package wg

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// Banc de débit du tunnel : deux nœuds complets dans le même processus, reliés
// en UDP sur 127.0.0.1. Les deux bouts (chiffrement et netstack) consomment le
// même processeur : le résultat est une borne basse du débit entre deux NAS de
// cette puissance.
//
//	deploy/go.sh test ./internal/wg -run '^$' -bench Tunnel -benchtime 5x
//	CERCLE_BENCH_MB=2048 deploy/go.sh test ./internal/wg -run TestThroughput -v
func BenchmarkTunnel(b *testing.B) {
	const size = 256 << 20
	a, c := benchPair(b, false)
	b.SetBytes(size)
	b.ResetTimer()
	for b.Loop() {
		transfer(b, a, c, size)
	}
}

// TestThroughput affiche le débit en Mb/s. Ignoré avec -short.
func TestThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("banc de débit ignoré avec -short")
	}
	mb := 512
	if v, err := strconv.Atoi(os.Getenv("CERCLE_BENCH_MB")); err == nil && v > 0 {
		mb = v
	}
	size := int64(mb) << 20
	for _, upstream := range []bool{false, true} {
		a, c := benchPair(t, upstream)
		transfer(t, a, c, 8<<20) // échauffement
		start := time.Now()
		transfer(t, a, c, size)
		el := time.Since(start)
		label := "netstack Cercle"
		if upstream {
			label = "netstack wireguard-go d'origine"
		}
		t.Logf("%s : %d Mio en %v = %.0f Mb/s", label, mb, el.Round(time.Millisecond), float64(size*8)/el.Seconds()/1e6)
	}

	// Référence : TCP du noyau sur la boucle locale, sans WireGuard.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go sink(l)
	start := time.Now()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	send(t, conn, size)
	el := time.Since(start)
	t.Logf("référence TCP noyau : %.0f Mb/s", float64(size*8)/el.Seconds()/1e6)
}

func benchPair(t testing.TB, upstream bool) (src, dst *node) {
	src = newNodeOpts(t, "10.42.0.1", upstream)
	dst = newNodeOpts(t, "10.42.0.2", upstream)
	link(t, src, dst)
	l, err := dst.dev.Listen(9000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go sink(l)
	return src, dst
}

// sink lit tout ce qu'on lui envoie et renvoie un octet à la fin de chaque connexion.
func sink(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}()
	}
}

func transfer(t testing.TB, src, dst *node, size int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := dial(ctx, src, dst.ip, 9000)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, size)
}

// send écrit size octets puis attend que le pair ait tout lu (fermeture).
func send(t testing.TB, c net.Conn, size int64) {
	t.Helper()
	buf := make([]byte, 256<<10)
	for left := size; left > 0; {
		n := int64(len(buf))
		if left < n {
			n = left
		}
		w, err := c.Write(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		left -= int64(w)
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		_, _ = io.Copy(io.Discard, c)
	}
	c.Close()
}
