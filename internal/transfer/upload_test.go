package transfer

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/config"
)

func TestAcquireMax(t *testing.T) {
	u := NewUploads(0, 2, nil)
	r1, _, ok1 := u.Acquire()
	r2, _, ok2 := u.Acquire()
	_, retry, ok3 := u.Acquire()
	if !ok1 || !ok2 || ok3 || retry != MaxUploadsRetry {
		t.Fatalf("places : %v %v %v, retry %v", ok1, ok2, ok3, retry)
	}
	r1()
	r1() // une libération en double ne rend pas deux places
	if _, _, ok := u.Acquire(); !ok {
		t.Fatal("place libérée non rendue")
	}
	if _, _, ok := u.Acquire(); ok {
		t.Fatal("libération en double comptée deux fois")
	}
	r2()
	u.SetMax(1)
	if _, _, _, active := u.Limits(); active != 1 {
		t.Fatalf("uploads actifs = %d", active)
	}
}

func TestAcquireWindow(t *testing.T) {
	w, _ := config.ParseWindow("01:00-07:00")
	u := NewUploads(0, 3, &w)
	u.now = func() time.Time { return time.Date(2026, 11, 4, 23, 30, 0, 0, time.UTC) }
	_, retry, ok := u.Acquire()
	if ok || retry != 90*time.Minute {
		t.Fatalf("hors créneau : ok=%v retry=%v ; attendu 1 h 30", ok, retry)
	}
	u.now = func() time.Time { return time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC) }
	if _, _, ok := u.Acquire(); !ok {
		t.Fatal("dans le créneau : refusé")
	}
	u.SetWindow(nil)
	u.now = func() time.Time { return time.Date(2026, 11, 4, 23, 30, 0, 0, time.UTC) }
	if _, _, ok := u.Acquire(); !ok {
		t.Fatal("créneau retiré : toujours refusé")
	}
}

// TestRateShared : trois lecteurs se partagent un débit total de 4 Mio/s.
func TestRateShared(t *testing.T) {
	const limit = 4 << 20
	u := NewUploads(limit, 3, nil)
	start := time.Now()
	var wg sync.WaitGroup
	var total int64
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		wg.Go(func() {
			r := u.Reader(context.Background(), bytes.NewReader(make([]byte, 3<<20)))
			n, _ := io.Copy(io.Discard, r)
			mu.Lock()
			total += n
			mu.Unlock()
		})
	}
	wg.Wait()
	rate := float64(total) / time.Since(start).Seconds()
	// 9 Mio à 4 Mio/s, moins la rafale initiale : un peu plus de 2 s.
	if rate < 0.9*limit || rate > 1.15*limit {
		t.Fatalf("débit total %.2f Mio/s, attendu environ 4", rate/(1<<20))
	}
}
