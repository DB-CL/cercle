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

// Tunnel throughput bench: two full nodes in one process, linked over UDP on
// 127.0.0.1. Both ends (encryption and netstack) use the same CPU: the result
// is a lower bound for two NAS boxes of that class.
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

// TestThroughput prints the throughput in Mb/s. Skipped with -short.
func TestThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput bench skipped with -short")
	}
	mb := 512
	if v, err := strconv.Atoi(os.Getenv("CERCLE_BENCH_MB")); err == nil && v > 0 {
		mb = v
	}
	size := int64(mb) << 20
	for _, upstream := range []bool{false, true} {
		a, c := benchPair(t, upstream)
		transfer(t, a, c, 8<<20) // warm-up
		start := time.Now()
		transfer(t, a, c, size)
		el := time.Since(start)
		label := "Cercle netstack"
		if upstream {
			label = "upstream wireguard-go netstack"
		}
		t.Logf("%s: %d MiB in %v = %.0f Mb/s", label, mb, el.Round(time.Millisecond), float64(size*8)/el.Seconds()/1e6)
	}

	// Reference: kernel TCP on loopback, without WireGuard.
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
	t.Logf("kernel TCP reference: %.0f Mb/s", float64(size*8)/el.Seconds()/1e6)
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

// sink reads everything it receives and closes each connection at the end.
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

// send writes size bytes then waits until the peer has read everything (close).
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
