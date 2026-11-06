package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseRate reads a rate in bytes per second: "0", "500K", "20M", "1G".
// Suffixes are powers of 1024 (20M = 20 MiB/s). Empty or 0 = unlimited.
func ParseRate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	}
	num := s
	if mult != 1 {
		num = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, errors.New("expected a rate in bytes/s with a K, M or G suffix (e.g. 20M)")
	}
	v := f * float64(mult)
	if v > math.MaxInt64/2 {
		return 0, errors.New("rate too large")
	}
	return int64(v), nil
}

// Window is a daily time window in local time. It may span midnight
// (22:00-06:00).
type Window struct {
	Start, End time.Duration // offsets from midnight
}

// ParseWindow reads an "HH:MM-HH:MM" window.
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Window{}, errors.New("expected a window as HH:MM-HH:MM")
	}
	start, err := parseClock(a)
	if err != nil {
		return Window{}, err
	}
	end, err := parseClock(b)
	if err != nil {
		return Window{}, err
	}
	if start == end {
		return Window{}, errors.New("empty window: start equals end")
	}
	return Window{Start: start, End: end}, nil
}

func parseClock(s string) (time.Duration, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("invalid time %q (expected HH:MM)", s)
	}
	return time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute, nil
}

func (w Window) String() string {
	f := func(d time.Duration) string {
		return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
	}
	return f(w.Start) + "-" + f(w.End)
}

// sinceMidnight returns the time elapsed since midnight in t's time zone.
func sinceMidnight(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second + time.Duration(t.Nanosecond())
}

// Contains reports whether t falls in the window (start included, end excluded).
func (w Window) Contains(t time.Time) bool {
	d := sinceMidnight(t)
	if w.Start < w.End {
		return d >= w.Start && d < w.End
	}
	return d >= w.Start || d < w.End
}

// NextOpen returns the next window start from t, or t itself if the window
// is already open.
func (w Window) NextOpen(t time.Time) time.Time {
	if w.Contains(t) {
		return t
	}
	y, mo, d := t.Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
	next := midnight.Add(w.Start)
	if !next.After(t) {
		next = time.Date(y, mo, d+1, 0, 0, 0, 0, t.Location()).Add(w.Start)
	}
	return next
}

// FormatRate writes a rate the way ParseRate reads it: "0", "512K", "20M", "1G".
func FormatRate(bps int64) string {
	switch {
	case bps <= 0:
		return "0"
	case bps%(1<<30) == 0:
		return strconv.FormatInt(bps>>30, 10) + "G"
	case bps%(1<<20) == 0:
		return strconv.FormatInt(bps>>20, 10) + "M"
	case bps%(1<<10) == 0:
		return strconv.FormatInt(bps>>10, 10) + "K"
	}
	return strconv.FormatInt(bps, 10)
}
