package peerapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/store"
	"github.com/DB-CL/cercle/internal/transfer"
	"github.com/DB-CL/cercle/internal/wg"
)

type shareFixture struct {
	srv     *httptest.Server
	uploads *transfer.Uploads
	ix      *index.Index
	share   string
	data    map[string][]byte
}

// newShare serves a test share over plain HTTP: the middleware sees
// 127.0.0.1, declared as a member. The tunnel is covered by peerapi_test.go.
func newShare(t *testing.T, files map[string]int) *shareFixture {
	t.Helper()
	base := t.TempDir()
	share := filepath.Join(base, "share")
	data := map[string][]byte{}
	for rel, size := range files {
		b := make([]byte, size)
		_, _ = rand.Read(b)
		p := filepath.Join(share, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		data[rel] = b
	}
	if err := os.WriteFile(filepath.Join(base, "secret"), []byte("outside the share"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "secret"), filepath.Join(share, "fuite")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(base, "cercle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ix := index.New(st, share, nil, nil)
	if _, err := ix.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	up := transfer.NewUploads(0, 3, nil)
	s := &Server{
		CircleID:  "c",
		Directory: staticDir{netip.MustParseAddr("127.0.0.1"): {Name: "local"}},
		Info:      func(context.Context) Info { return Info{} },
		Index:     ix,
		Uploads:   up,
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &shareFixture{srv: srv, uploads: up, ix: ix, share: share, data: data}
}

func (f *shareFixture) get(t *testing.T, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/api/v1/file?path="+path, nil)
	req.Header.Set(CircleHeader, "c")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, r *http.Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFileRangeAndETag(t *testing.T) {
	f := newShare(t, map[string]int{"films/a.bin": 1 << 20, ".cache/x": 10})
	want := f.data["films/a.bin"]

	r := f.get(t, "films/a.bin", nil)
	etag := r.Header.Get("ETag")
	if got := body(t, r); r.StatusCode != 200 || !bytes.Equal(got, want) || etag == "" {
		t.Fatalf("whole file: %d, %d bytes, ETag %q", r.StatusCode, len(got), etag)
	}
	if r.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("Accept-Ranges missing")
	}

	r = f.get(t, "films/a.bin", map[string]string{"Range": "bytes=1000-", "If-Range": etag})
	if got := body(t, r); r.StatusCode != http.StatusPartialContent || !bytes.Equal(got, want[1000:]) {
		t.Fatalf("resume: %d, %d bytes", r.StatusCode, len(got))
	}

	// File changed since: If-Range no longer matches, the file restarts from zero.
	r = f.get(t, "films/a.bin", map[string]string{"Range": "bytes=1000-", "If-Range": `"1-1"`})
	if got := body(t, r); r.StatusCode != 200 || len(got) != len(want) {
		t.Fatalf("stale ETag: %d, %d bytes; want 200 and the whole file", r.StatusCode, len(got))
	}

	for path, code := range map[string]int{
		"../../etc/passwd": 400, "/etc/passwd": 400, "films/../../secret": 400,
		"fuite": 404, ".cache/x": 404, "films": 404, "absent": 404,
	} {
		r := f.get(t, path, nil)
		body(t, r)
		if r.StatusCode != code {
			t.Errorf("%s: %d, want %d", path, r.StatusCode, code)
		}
	}
}

// TestUploadLimits covers two acceptance criteria: UPLOAD_LIMIT=10M gives 9 to
// 11 MiB/s in total with 3 downloaders, and the 4th gets 503 + Retry-After.
func TestUploadLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped with -short (3 s)")
	}
	const size = 8 << 20
	f := newShare(t, map[string]int{"a": size, "b": size, "c": size, "d": 10})
	f.uploads.SetLimit(10 << 20)

	start := time.Now()
	var wg sync.WaitGroup
	results := make(chan int, 3)
	opened := make(chan struct{}, 3)
	for _, p := range []string{"a", "b", "c"} {
		wg.Go(func() {
			r := f.get(t, p, nil)
			opened <- struct{}{}
			results <- len(body(t, r))
		})
	}
	for range 3 {
		<-opened
	}
	r := f.get(t, "d", nil)
	body(t, r)
	if r.StatusCode != http.StatusServiceUnavailable || r.Header.Get("Retry-After") != "60" {
		t.Errorf("4th downloader: %d, Retry-After %q; want 503 and 60", r.StatusCode, r.Header.Get("Retry-After"))
	}
	wg.Wait()
	close(results)
	total := 0
	for n := range results {
		total += n
	}
	rate := float64(total) / time.Since(start).Seconds() / (1 << 20)
	t.Logf("total rate: %.2f MiB/s", rate)
	if total != 3*size || rate < 9 || rate > 11 {
		t.Errorf("total rate %.2f MiB/s (%d bytes), want 9 to 11", rate, total)
	}

	// Slot released: the 4th goes through.
	r = f.get(t, "d", nil)
	if body(t, r); r.StatusCode != 200 {
		t.Errorf("after release: %d", r.StatusCode)
	}
}

func TestFilesAndTreeHTTP(t *testing.T) {
	f := newShare(t, map[string]int{"films/a.bin": 3, "films/b.bin": 4, "z.txt": 1})
	get := func(path string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
		req.Header.Set(CircleHeader, "c")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r, string(body(t, r))
	}
	if r, b := get("/api/v1/files?path="); r.StatusCode != 200 || !bytes.Contains([]byte(b), []byte(`"name":"films"`)) {
		t.Errorf("files at root: %d %s", r.StatusCode, b)
	}
	if r, b := get("/api/v1/tree?path=films"); r.StatusCode != 200 || bytes.Count([]byte(b), []byte("\n")) != 2 {
		t.Errorf("tree : %d %s", r.StatusCode, b)
	}
	if r, _ := get("/api/v1/files?path=../x"); r.StatusCode != 400 {
		t.Errorf("files with ..: %d", r.StatusCode)
	}
	if r, _ := get("/api/v1/files?path=films&cursor=zzzz"); r.StatusCode != 400 {
		t.Errorf("invalid cursor: %d", r.StatusCode)
	}
	if r, _ := get("/api/v1/tree?path=absent"); r.StatusCode != 404 {
		t.Errorf("missing tree: %d", r.StatusCode)
	}
}

// TestBrowseThroughTunnel: browsing with the client, through WireGuard.
func TestBrowseThroughTunnel(t *testing.T) {
	alice := startNode(t, "alice", "10.42.0.1")
	bob := startNode(t, "bob", "10.42.0.2")
	if err := alice.dev.SetPeers([]wg.Peer{bob.peer()}); err != nil {
		t.Fatal(err)
	}
	if err := bob.dev.SetPeers([]wg.Peer{alice.peer()}); err != nil {
		t.Fatal(err)
	}
	f := newShare(t, map[string]int{"films/a.bin": 3, "films/b.bin": 4})
	l, err := alice.dev.Listen(Port)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{CircleID: "c", Directory: staticDir{bob.ip: bob.member()}, Index: f.ix, Uploads: f.uploads,
		Info: func(context.Context) Info { return Info{} }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	defer func() { cancel(); <-done }()

	c := NewClient(bob.dev.DialContext, "c")
	// First contact: the WireGuard handshake may be lost and only retried after
	// 5 s (possibly twice). Wait for the link, 30 s at most.
	deadline := time.Now().Add(30 * time.Second)
	for {
		ictx, icancel := context.WithTimeout(context.Background(), 6*time.Second)
		_, err := c.Info(ictx, alice.ip)
		icancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel never established: %v", err)
		}
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rcancel()
	page, err := c.Files(rctx, alice.ip, "films", "")
	if err != nil || len(page.Entries) != 2 {
		t.Fatalf("Files = %+v, %v", page, err)
	}
	var paths []string
	err = c.Tree(rctx, alice.ip, "", func(e index.Entry) error { paths = append(paths, e.Path); return nil })
	if err != nil || fmt.Sprint(paths) != "[films/a.bin films/b.bin]" {
		t.Fatalf("Tree = %v, %v", paths, err)
	}
	_, err = c.Files(rctx, alice.ip, "absent", "")
	if se, ok := err.(*StatusError); !ok || se.Code != 404 {
		t.Fatalf("missing folder: %v", err)
	}
}
