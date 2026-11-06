package transfer

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := Backoff(i+1, 30*time.Second, 15*time.Minute); got != w {
			t.Errorf("attempt %d: %v, want %v", i+1, got, w)
		}
	}
	if got := Backoff(1000, 30*time.Second, 15*time.Minute); got != 15*time.Minute {
		t.Errorf("cap: %v", got)
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"alice": "alice", " Bob the NAS ": "Bob the NAS", "../x": "_x", "a/b\\c": "a_b_c",
		"..": "peer-abcdefgh", "": "peer-abcdefgh", ".cache": "cache", "é\x01té": "été",
	}
	for in, want := range cases {
		if got := SafeName(in, "abcdefghijk"); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeName("", "ab/c+d"); got != "peer-ab_c-d" {
		t.Errorf("fallback to the ID: %q", got)
	}
}

func TestParseContentRange(t *testing.T) {
	start, size, err := parseContentRange("bytes 1000-4095/4096")
	if err != nil || start != 1000 || size != 4096 {
		t.Fatalf("= %d, %d, %v", start, size, err)
	}
	for _, bad := range []string{"", "bytes */4096", "items 1-2/3", "bytes 1-2"} {
		if _, _, err := parseContentRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
