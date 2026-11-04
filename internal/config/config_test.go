package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"NODE_NAME": "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeName != "alice" || c.WGListenPort != 51820 || c.UIListen != ":8080" ||
		c.MaxUploads != 3 || c.DownloadConcurrency != 2 || c.ScanInterval != time.Hour ||
		c.InviteTTL != 72*time.Hour || c.LogLevel != slog.LevelInfo ||
		c.TunnelSubnet.String() != "10.42.0.0/24" || c.UploadLimit != 0 || c.UploadWindow != nil {
		t.Fatalf("valeurs par défaut inattendues : %+v", c)
	}
}

func TestLoadValues(t *testing.T) {
	c, err := Load(env(map[string]string{
		"WG_LISTEN_PORT":      "51821",
		"WG_PUBLIC_ENDPOINTS": "alice.duckdns.org:51820, [2001:db8::1]:51820,203.0.113.4:4000",
		"TUNNEL_SUBNET":       "10.77.3.9/24",
		"UPLOAD_LIMIT":        "20M",
		"UPLOAD_WINDOW":       "01:00-07:00",
		"SHARE_EXCLUDE":       "*.tmp, Thumbs.db",
		"LOG_LEVEL":           "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.WGListenPort != 51821 || len(c.WGPublicEndpoints) != 3 || c.UploadLimit != 20<<20 ||
		c.UploadWindow.String() != "01:00-07:00" || len(c.ShareExclude) != 2 ||
		c.LogLevel != slog.LevelDebug || c.TunnelSubnet.String() != "10.77.3.0/24" {
		t.Fatalf("valeurs inattendues : %+v", c)
	}
}

func TestLoadErrorsAreAllReported(t *testing.T) {
	_, err := Load(env(map[string]string{
		"WG_LISTEN_PORT":      "99999",
		"UPLOAD_LIMIT":        "vite",
		"UPLOAD_WINDOW":       "25:00-07:00",
		"WG_PUBLIC_ENDPOINTS": "sans-port",
		"TUNNEL_SUBNET":       "fd00::/64",
		"MAX_UPLOADS":         "0",
		"SCAN_INTERVAL":       "-1h",
	}))
	if err == nil {
		t.Fatal("erreur attendue")
	}
	for _, name := range []string{"WG_LISTEN_PORT", "UPLOAD_LIMIT", "UPLOAD_WINDOW", "WG_PUBLIC_ENDPOINTS", "TUNNEL_SUBNET", "MAX_UPLOADS", "SCAN_INTERVAL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("l'erreur ne mentionne pas %s : %v", name, err)
		}
	}
}

func TestParseRate(t *testing.T) {
	cases := map[string]int64{
		"": 0, "0": 0, "1024": 1024, "500K": 500 << 10, "500k": 500 << 10,
		"20M": 20 << 20, "1.5M": 3 << 19, "1G": 1 << 30, " 10M ": 10 << 20,
	}
	for in, want := range cases {
		got, err := ParseRate(in)
		if err != nil || got != want {
			t.Errorf("ParseRate(%q) = %d, %v ; attendu %d", in, got, err, want)
		}
	}
	for _, in := range []string{"M", "-1M", "10T", "abc", "NaN"} {
		if _, err := ParseRate(in); err == nil {
			t.Errorf("ParseRate(%q) : erreur attendue", in)
		}
	}
}

func at(h, m int) time.Time {
	return time.Date(2026, 11, 4, h, m, 0, 0, time.UTC)
}

func TestWindow(t *testing.T) {
	night, err := ParseWindow("22:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	day, _ := ParseWindow("01:00-07:00")

	contains := []struct {
		w    Window
		t    time.Time
		want bool
	}{
		{day, at(0, 59), false}, {day, at(1, 0), true}, {day, at(6, 59), true}, {day, at(7, 0), false},
		{night, at(21, 59), false}, {night, at(22, 0), true}, {night, at(3, 0), true}, {night, at(6, 0), false},
	}
	for _, c := range contains {
		if got := c.w.Contains(c.t); got != c.want {
			t.Errorf("%v.Contains(%v) = %v", c.w, c.t.Format("15:04"), got)
		}
	}

	if got := day.NextOpen(at(12, 0)); !got.Equal(time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("NextOpen après le créneau = %v", got)
	}
	if got := day.NextOpen(at(0, 30)); !got.Equal(at(1, 0)) {
		t.Errorf("NextOpen avant le créneau = %v", got)
	}
	if got := night.NextOpen(at(23, 0)); !got.Equal(at(23, 0)) {
		t.Errorf("NextOpen dans le créneau = %v", got)
	}
	if got := night.NextOpen(at(7, 0)); !got.Equal(at(22, 0)) {
		t.Errorf("NextOpen de jour = %v", got)
	}

	for _, bad := range []string{"01:00", "01:00-01:00", "1h-2h", "24:30-01:00", "aa:00-01:00"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) : erreur attendue", bad)
		}
	}
	if _, err := ParseWindow("18:00-24:00"); err != nil {
		t.Errorf("24:00 doit être accepté comme fin : %v", err)
	}
}

func TestApplySettings(t *testing.T) {
	c, err := Load(env(map[string]string{"UPLOAD_LIMIT": "10M", "UPLOAD_WINDOW": "01:00-07:00", "MAX_UPLOADS": "3"}))
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]string{SettingUploadLimit: "20M", SettingUploadWindow: "", SettingNodeName: "alice-nas"}
	err = c.ApplySettings(func(k string) (string, bool, error) { v, ok := stored[k]; return v, ok, nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.UploadLimit != 20<<20 || c.UploadWindow != nil || c.MaxUploads != 3 || c.NodeName != "alice-nas" {
		t.Fatalf("après réglages : %+v", c)
	}
	stored[SettingMaxUploads] = "zéro"
	if err := c.ApplySettings(func(k string) (string, bool, error) { v, ok := stored[k]; return v, ok, nil }); err == nil {
		t.Fatal("réglage invalide accepté")
	}
}
