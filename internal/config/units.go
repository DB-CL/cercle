package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseRate lit un débit en octets par seconde : « 0 », « 500K », « 20M », « 1G ».
// Les suffixes sont en puissances de 1024 (20M = 20 Mio/s). Vide ou 0 = illimité.
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
		return 0, errors.New("débit attendu en octets/s, suffixes K, M ou G (ex. 20M)")
	}
	v := f * float64(mult)
	if v > math.MaxInt64/2 {
		return 0, errors.New("débit trop grand")
	}
	return int64(v), nil
}

// Window est un créneau horaire quotidien, en heure locale. Il peut passer
// minuit (22:00-06:00).
type Window struct {
	Start, End time.Duration // décalages depuis minuit
}

// ParseWindow lit un créneau « HH:MM-HH:MM ».
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Window{}, errors.New("créneau attendu au format HH:MM-HH:MM")
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
		return Window{}, errors.New("créneau vide : début et fin identiques")
	}
	return Window{Start: start, End: end}, nil
}

func parseClock(s string) (time.Duration, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("heure invalide %q (HH:MM attendu)", s)
	}
	return time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute, nil
}

func (w Window) String() string {
	f := func(d time.Duration) string {
		return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
	}
	return f(w.Start) + "-" + f(w.End)
}

// sinceMidnight renvoie le temps écoulé depuis minuit dans le fuseau de t.
func sinceMidnight(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second + time.Duration(t.Nanosecond())
}

// Contains dit si t tombe dans le créneau (début inclus, fin exclue).
func (w Window) Contains(t time.Time) bool {
	d := sinceMidnight(t)
	if w.Start < w.End {
		return d >= w.Start && d < w.End
	}
	return d >= w.Start || d < w.End
}

// NextOpen renvoie le prochain début de créneau à partir de t, ou t lui-même
// si le créneau est déjà ouvert.
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

// FormatRate écrit un débit comme ParseRate le lit : « 0 », « 512K », « 20M », « 1G ».
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
