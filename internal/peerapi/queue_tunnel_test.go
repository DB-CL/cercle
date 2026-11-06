package peerapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/store"
	"github.com/DB-CL/cercle/internal/transfer"
	"github.com/DB-CL/cercle/internal/wg"
)

// TestQueueThroughTunnel: scaled-down version of the criterion "a folder of
// 10,000 files is queued in one click and fully downloaded with identical
// checksums", through WireGuard. CERCLE_QUEUE_FILES changes the file count.
func TestQueueThroughTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped with -short")
	}
	n := 500
	if v, err := strconv.Atoi(os.Getenv("CERCLE_QUEUE_FILES")); err == nil && v > 0 {
		n = v
	}
	files := map[string]int{}
	for i := range n {
		files[fmt.Sprintf("photos/%02d/img-%05d.jpg", i%20, i)] = 1000 + i*37%50000
	}
	files["photos/gros.mkv"] = 20 << 20
	f := newShare(t, files)

	alice := startNode(t, "alice", "10.42.0.1")
	bob := startNode(t, "bob", "10.42.0.2")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(alice.dev.SetPeers([]wg.Peer{bob.peer()}))
	must(bob.dev.SetPeers([]wg.Peer{alice.peer()}))
	l, err := alice.dev.Listen(Port)
	must(err)
	s := &Server{CircleID: "c", Directory: staticDir{bob.ip: bob.member()}, Index: f.ix, Uploads: f.uploads,
		Info: func(context.Context) Info { return Info{} }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, l) }()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "bob.db"))
	must(err)
	defer st.Close()
	dl := t.TempDir()
	aliceID := alice.key.Public().String()
	m := transfer.NewManager(transfer.Options{
		Store:  st,
		Remote: NewClient(bob.dev.DialContext, "c"),
		Peers: func(id string) (transfer.Peer, bool) {
			return transfer.Peer{ID: aliceID, Name: "alice", IP: alice.ip}, id == aliceID
		},
		Dir:         dl,
		Concurrency: 2,
	})
	runDone := make(chan error, 1)
	mctx, mcancel := context.WithCancel(ctx)
	go func() { runDone <- m.Run(mctx) }()

	start := time.Now()
	added, skipped, err := m.Enqueue(ctx, aliceID, []string{"photos"})
	must(err)
	if added != n+1 || skipped != 0 {
		t.Fatalf("added %d, refused %d; want %d and 0", added, skipped, n+1)
	}
	enqueued := time.Since(start)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		items, err := m.List(ctx)
		must(err)
		left := 0
		for _, it := range items {
			if it.Status == transfer.Failed {
				t.Fatalf("item in error: %+v", it)
			}
			if it.Status != transfer.Done {
				left++
			}
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d items left after 5 min", left)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("%d files: queued in %v, fully downloaded in %v", n+1, enqueued.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	mcancel()
	must(<-runDone)

	for rel, want := range f.data {
		got, err := os.ReadFile(filepath.Join(dl, "alice", rel))
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(got) != sha256.Sum256(want) {
			t.Fatalf("%s: checksum mismatch", rel)
		}
	}
	cancel()
	must(<-served)
}
