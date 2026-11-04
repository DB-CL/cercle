package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/store"
)

func TestCleanPath(t *testing.T) {
	ok := map[string]string{
		"": "", ".": "", "films": "films", "films/": "films", "films//a.mkv": "films/a.mkv",
		"./films/./a.mkv": "films/a.mkv", "é t/ü": "é t/ü", `dos\sier`: `dos\sier`,
	}
	for in, want := range ok {
		got, err := CleanPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v ; attendu %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/etc/passwd", "../../etc/passwd", "films/../../x", "films/..", "..", "a\x00b"} {
		if _, err := CleanPath(in); !errors.Is(err, ErrBadPath) {
			t.Errorf("CleanPath(%q) : refus attendu, obtenu %v", in, err)
		}
	}
}

func write(t testing.TB, root, rel string, size int) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newIndex(t testing.TB, share string, excludes ...string) *Index {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cercle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, share, excludes, nil)
}

func names(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func TestScanListTreeOpen(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	share := filepath.Join(base, "share")
	write(t, share, "films/b.mkv", 100)
	write(t, share, "films/A.mkv", 50)
	write(t, share, "films/série/e01.mkv", 10)
	write(t, share, "films/série/e02.mkv", 20)
	write(t, share, "musique/z.flac", 5)
	write(t, share, "lisez-moi.txt", 1)
	// Exclus.
	write(t, share, ".cache/x", 1000)
	write(t, share, "films/.secret", 1000)
	write(t, share, "@eaDir/thumb.jpg", 1000)
	write(t, share, "#recycle/old.mkv", 1000)
	write(t, share, ".Trash-1000/old", 1000)
	write(t, share, "films/brouillon.tmp", 1000)
	// Liens : interne vers un fichier (gardé), sortant, vers un dossier (ignorés).
	write(t, base, "dehors/passwd", 1000)
	must(t, os.Symlink("../films/A.mkv", filepath.Join(share, "musique/lien.mkv")))
	must(t, os.Symlink(filepath.Join(base, "dehors/passwd"), filepath.Join(share, "fuite")))
	must(t, os.Symlink("films", filepath.Join(share, "boucle")))

	ix := newIndex(t, share, "*.tmp")
	st, err := ix.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 7 || st.Dirs != 3 || st.Size != 236 {
		t.Fatalf("scan = %+v ; attendu 7 fichiers, 3 dossiers, 236 octets", st)
	}

	root, next, err := ix.List(ctx, "", "", 0)
	if err != nil || next != "" {
		t.Fatal(err, next)
	}
	if fmt.Sprint(names(root)) != "[films musique lisez-moi.txt]" {
		t.Errorf("racine = %v", names(root))
	}
	films, _, _ := ix.List(ctx, "films", "", 0)
	if fmt.Sprint(names(films)) != "[série A.mkv b.mkv]" {
		t.Errorf("films = %v (dossiers d'abord, puis nom sans casse)", names(films))
	}
	if films[0].Size != 30 || films[0].FileCount != 2 {
		t.Errorf("totaux de série = %+v", films[0])
	}
	if size, n, last := ix.Totals(ctx); size != 236 || n != 7 || last == nil {
		t.Errorf("Totals = %d, %d, %v", size, n, last)
	}

	// Pagination une entrée à la fois.
	var paged []string
	cur := ""
	for {
		page, next, err := ix.List(ctx, "films", cur, 1)
		if err != nil {
			t.Fatal(err)
		}
		paged = append(paged, names(page)...)
		if next == "" {
			break
		}
		cur = next
	}
	if fmt.Sprint(paged) != fmt.Sprint(names(films)) {
		t.Errorf("pagination = %v", paged)
	}

	var tree []string
	must(t, ix.Tree(ctx, "films", func(e Entry) error { tree = append(tree, e.Path); return nil }))
	if fmt.Sprint(tree) != "[films/A.mkv films/b.mkv films/série/e01.mkv films/série/e02.mkv]" {
		t.Errorf("tree = %v", tree)
	}

	f, info, err := ix.Open(ctx, "musique/lien.mkv")
	if err != nil || info.Size() != 50 {
		t.Fatalf("lien interne : %v", err)
	}
	f.Close()
	for _, p := range []string{"fuite", "films/.secret", "films/brouillon.tmp", "boucle/A.mkv", "films", "absent"} {
		if f, _, err := ix.Open(ctx, p); err == nil {
			f.Close()
			t.Errorf("Open(%q) aurait dû échouer", p)
		}
	}
	if _, _, err := ix.List(ctx, "lisez-moi.txt", "", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("List d'un fichier : %v", err)
	}

	// Rescan : modification, suppression, ajout.
	write(t, share, "films/b.mkv", 300)
	must(t, os.RemoveAll(filepath.Join(share, "films/série")))
	write(t, share, "musique/nouveau.flac", 7)
	if _, err := ix.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if e, err := ix.Lookup(ctx, "films/b.mkv"); err != nil || e.Size != 300 {
		t.Errorf("modification non vue : %+v, %v", e, err)
	}
	if _, err := ix.Lookup(ctx, "films/série/e01.mkv"); !errors.Is(err, ErrNotFound) {
		t.Errorf("suppression non vue : %v", err)
	}
	if size, n, _ := ix.Totals(ctx); n != 6 || size != 50+300+5+1+7+50 {
		t.Errorf("totaux après rescan = %d octets, %d fichiers", size, n)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestScanScale vérifie la mémoire et la vitesse sur un gros partage.
// CERCLE_SCAN_FILES=500000 pour la cible du PRD (long : création des fichiers).
func TestScanScale(t *testing.T) {
	if testing.Short() {
		t.Skip("ignoré avec -short")
	}
	n := 20000
	if v, err := strconv.Atoi(os.Getenv("CERCLE_SCAN_FILES")); err == nil && v > 0 {
		n = v
	}
	share := t.TempDir()
	for i := 0; i < n; i++ {
		dir := filepath.Join(share, fmt.Sprintf("d%03d/s%03d", i%100, (i/100)%100))
		if i < 10000 {
			must(t, os.MkdirAll(dir, 0o755))
		}
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("fichier-%07d.bin", i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	ix := newIndex(t, share)
	ctx := context.Background()

	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.Sys > peak {
				peak = ms.Sys
			}
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	st, err := ix.Scan(ctx)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != int64(n) {
		t.Fatalf("%d fichiers indexés sur %d", st.Files, n)
	}
	start := time.Now()
	page, _, err := ix.List(ctx, "d042/s007", "", 0)
	listDur := time.Since(start)
	if err != nil || len(page) == 0 {
		t.Fatal(err)
	}
	start = time.Now()
	_, err = ix.Scan(ctx)
	rescan := time.Since(start)
	must(t, err)
	t.Logf("%d fichiers : scan %v, rescan %v, mémoire max %d Mio, liste d'un dossier %v",
		n, st.Duration.Round(time.Millisecond), rescan.Round(time.Millisecond), peak>>20, listDur)
	if peak > 200<<20 {
		t.Errorf("mémoire max %d Mio > 200 Mio", peak>>20)
	}
	if listDur > 200*time.Millisecond {
		t.Errorf("liste d'un dossier en %v > 200 ms", listDur)
	}
}
