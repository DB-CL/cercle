package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/node"
)

type tnode struct {
	t   *testing.T
	n   *node.Node
	srv *httptest.Server
	cfg config.Config
	ui  *Server
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func start(t *testing.T, name, password string, files map[string]string) *tnode {
	t.Helper()
	base := t.TempDir()
	cfg, _ := config.Load(func(string) string { return "" })
	port := freeUDPPort(t)
	cfg.NodeName, cfg.WGListenPort = name, port
	cfg.WGPublicEndpoints = []string{fmt.Sprintf("127.0.0.1:%d", port)}
	cfg.DataDir, cfg.ShareDir, cfg.DownloadsDir = filepath.Join(base, "data"), filepath.Join(base, "share"), filepath.Join(base, "dl")
	for rel, content := range files {
		p := filepath.Join(cfg.ShareDir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(cfg.ShareDir, 0o755)
	n, err := node.New(context.Background(), node.Options{Config: cfg, GossipInterval: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	ui := &Server{Node: n, Password: password}
	ui.authState.failWait = 20 * time.Millisecond
	srv := httptest.NewServer(ui.Handler())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
		n.Close()
	})
	return &tnode{t: t, n: n, srv: srv, cfg: cfg, ui: ui}
}

// call makes a request to the interface API; out receives the JSON.
func (tn *tnode) call(method, path string, body, out any) int {
	tn.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, tn.srv.URL+"/ui/api"+path, r)
	req.Header.Set(CSRFHeader, "1")
	req.SetBasicAuth("", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tn.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStaticAndSecurity(t *testing.T) {
	a := start(t, "alice", "secret", nil)
	get := func(path string, auth bool) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, a.srv.URL+path, nil)
		if auth {
			req.SetBasicAuth("x", "secret")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// The page and its files are public (no secrets); the API is not, and never
	// triggers the browser's Basic dialog.
	r := get("/", false)
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), "<title>Cercle</title>") {
		t.Fatalf("page: %d", r.StatusCode)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("missing CSP: %q", csp)
	}
	r = get("/ui/api/status", false)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") != "" || body["login"] != true {
		t.Errorf("API without login: %d, WWW-Authenticate %q, %v", r.StatusCode, r.Header.Get("WWW-Authenticate"), body)
	}
	if r := get("/ui/api/status", true); r.StatusCode != 200 {
		t.Errorf("API with Basic (scripts): %d", r.StatusCode)
	}
	for _, f := range []string{"/app.js", "/style.css"} {
		if r := get(f, false); r.StatusCode != 200 {
			t.Errorf("%s: %d", f, r.StatusCode)
		}
	}
	if r := get("/healthz", false); r.StatusCode != 200 {
		t.Errorf("healthz: %d", r.StatusCode)
	}

	// CSRF: a change without the header is refused, even when authenticated.
	req, _ := http.NewRequest(http.MethodPost, a.srv.URL+"/ui/api/circle", nil)
	req.SetBasicAuth("x", "secret")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without %s: %d, want 403", CSRFHeader, resp.StatusCode)
	}
	if a.n.Membership().InCircle() {
		t.Fatal("circle created despite the refusal")
	}
}

func TestLoginSession(t *testing.T) {
	a := start(t, "alice", "secret", nil)
	post := func(path string, body any, cookies ...*http.Cookie) *http.Response {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, a.srv.URL+path, bytes.NewReader(b))
		req.Header.Set(CSRFHeader, "1")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	status := func(c *http.Cookie) int {
		req, _ := http.NewRequest(http.MethodGet, a.srv.URL+"/ui/api/status", nil)
		if c != nil {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if r := post("/ui/api/login", map[string]any{"password": "wrong"}); r.StatusCode != 401 || len(r.Cookies()) != 0 {
		t.Fatalf("wrong password: %d, %d cookies", r.StatusCode, len(r.Cookies()))
	}
	r := post("/ui/api/login", map[string]any{"password": "secret", "remember": true})
	if r.StatusCode != 204 || len(r.Cookies()) != 1 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	c := r.Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge < 29*24*3600 {
		t.Errorf("cookie: %+v", c)
	}
	if code := status(c); code != 200 {
		t.Fatalf("with the cookie: %d", code)
	}
	forged := *c
	forged.Value = strings.Replace(c.Value, ".", "9.", 1) // expiry pushed back, signature unchanged
	if code := status(&forged); code != 401 {
		t.Errorf("forged cookie: %d", code)
	}
	expired := &http.Cookie{Name: sessionCookie, Value: a.ui.sign(time.Now().Add(-time.Minute).Unix())}
	if code := status(expired); code != 401 {
		t.Errorf("expired cookie: %d", code)
	}
	// Browser session (without "stay signed in"): no Max-Age.
	if r := post("/ui/api/login", map[string]any{"password": "secret"}); r.Cookies()[0].MaxAge != 0 {
		t.Errorf("short session is persistent: %+v", r.Cookies()[0])
	}
	// Password change: existing sessions are dropped.
	other := &Server{Node: a.n, Password: "nouveau"}
	srv2 := httptest.NewServer(other.Handler())
	defer srv2.Close()
	req, _ := http.NewRequest(http.MethodGet, srv2.URL+"/ui/api/status", nil)
	req.AddCookie(c)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("cookie accepted after a password change: %d", resp.StatusCode)
	}
	// Logout: the cookie is cleared.
	if r := post("/ui/api/logout", nil, c); len(r.Cookies()) != 1 || r.Cookies()[0].MaxAge >= 0 {
		t.Errorf("logout: %+v", r.Cookies())
	}
	// Login without the anti-CSRF header: refused.
	b, _ := json.Marshal(map[string]string{"password": "secret"})
	resp, _ = http.Post(a.srv.URL+"/ui/api/login", "application/json", bytes.NewReader(b))
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("login without %s: %d", CSRFHeader, resp.StatusCode)
	}
}

func TestFullFlow(t *testing.T) {
	alice := start(t, "alice", "secret", map[string]string{"movies/a.mkv": "AAAA", "movies/b.mkv": "BB", "readme.txt": "hello"})
	bob := start(t, "bob", "secret", nil)

	var st statusResp
	if code := alice.call("POST", "/circle", nil, &st); code != 200 || st.Self == nil || st.Self.TunnelIP.String() != "10.42.0.1" {
		t.Fatalf("creation: %d %+v", code, st)
	}
	if code := alice.call("POST", "/circle", nil, nil); code != http.StatusConflict {
		t.Errorf("second creation: %d", code)
	}
	var inv struct {
		Code string `json:"code"`
	}
	if code := alice.call("POST", "/invites", nil, &inv); code != 201 || !strings.HasPrefix(inv.Code, "cercle1:") {
		t.Fatalf("invitation: %d", code)
	}
	if code := bob.call("POST", "/join", map[string]string{"code": inv.Code}, &st); code != 200 || !st.InCircle {
		t.Fatalf("join: %d %+v", code, st)
	}
	var e map[string]string
	if code := bob.call("POST", "/join", map[string]string{"code": inv.Code}, &e); code != http.StatusConflict {
		t.Errorf("second join: %d %v", code, e)
	}

	var aliceID string
	eventually(t, "alice online at bob", func() bool {
		var ms []node.PeerState
		bob.call("GET", "/members", nil, &ms)
		for _, m := range ms {
			if m.Name == "alice" && m.Online {
				aliceID = m.NodeID
				return true
			}
		}
		return false
	})
	if _, err := alice.n.Index().Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	var page struct {
		Entries []struct {
			Path  string `json:"path"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
		} `json:"entries"`
	}
	if code := bob.call("GET", "/members/"+url.PathEscape(aliceID)+"/files?path=", nil, &page); code != 200 || len(page.Entries) != 2 || !page.Entries[0].IsDir || page.Entries[0].Size != 6 {
		t.Fatalf("browsing: %d %+v", code, page)
	}
	var added map[string]int
	if code := bob.call("POST", "/queue", map[string]any{"peer": aliceID, "paths": []string{"movies", "readme.txt"}}, &added); code != 200 || added["added"] != 3 {
		t.Fatalf("queuing: %d %v", code, added)
	}
	eventually(t, "downloads finished", func() bool {
		var q queueResp
		bob.call("GET", "/queue?status=done", nil, &q)
		return q.Summary.Count["done"] == 3
	})
	got, _ := os.ReadFile(filepath.Join(bob.cfg.DownloadsDir, "alice", "movies", "a.mkv"))
	if string(got) != "AAAA" {
		t.Fatalf("received file: %q", got)
	}
	var cleared map[string]int64
	bob.call("POST", "/queue/actions", map[string]string{"action": "clear_done"}, &cleared)
	if cleared["removed"] != 3 {
		t.Errorf("clearing finished items: %v", cleared)
	}

	// Settings: refused field by field, then applied live and the name propagated.
	var bad struct {
		Fields map[string]string `json:"fields"`
	}
	s := bob.n.Settings()
	s.UploadLimit, s.DownloadWindow = "fast", "25:00-01:00"
	if code := bob.call("PUT", "/settings", s, &bad); code != http.StatusUnprocessableEntity || bad.Fields["upload_limit"] == "" || bad.Fields["download_window"] == "" {
		t.Fatalf("invalid settings: %d %+v", code, bad)
	}
	s = bob.n.Settings()
	s.Name, s.UploadLimit, s.MaxUploads, s.DownloadWindow = "bob-nas", "5M", 2, "01:00-07:00"
	var applied node.Settings
	if code := bob.call("PUT", "/settings", s, &applied); code != 200 || applied.UploadLimit != "5M" || applied.DownloadWindow != "01:00-07:00" {
		t.Fatalf("settings: %d %+v", code, applied)
	}
	if limit, max, _, _ := bob.n.Uploads().Limits(); limit != 5<<20 || max != 2 {
		t.Errorf("limits not applied: %d, %d", limit, max)
	}
	eventually(t, "bob's new name at alice", func() bool {
		var ms []node.PeerState
		alice.call("GET", "/members", nil, &ms)
		for _, m := range ms {
			if m.Name == "bob-nas" {
				return true
			}
		}
		return false
	})
}

func TestShareSelection(t *testing.T) {
	a := start(t, "alice", "secret", map[string]string{"movies/a.mkv": "AAAA", "music/b.flac": "BB", "photos/c.jpg": "C"})
	if _, err := a.n.Index().Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	type shareResp struct {
		All     bool `json:"all"`
		Files   int  `json:"files"`
		Folders []struct {
			Name   string `json:"name"`
			Shared bool   `json:"shared"`
		} `json:"folders"`
	}
	var sr shareResp
	if code := a.call("GET", "/share", nil, &sr); code != 200 || !sr.All || len(sr.Folders) != 3 || sr.Files != 3 {
		t.Fatalf("default share: %d %+v", code, sr)
	}
	var e map[string]string
	if code := a.call("PUT", "/share", map[string]any{"folders": []string{"../etc"}}, &e); code != 400 {
		t.Errorf("booby-trapped name: %d %v", code, e)
	}
	if code := a.call("PUT", "/share", map[string]any{"all": false, "folders": []string{"movies"}}, &sr); code != 200 || sr.All {
		t.Fatalf("selection: %d %+v", code, sr)
	}
	eventually(t, "reindexing", func() bool {
		a.call("GET", "/share", nil, &sr)
		return sr.Files == 1
	})
	for _, f := range sr.Folders {
		if f.Shared != (f.Name == "movies") {
			t.Errorf("%s: shared=%v", f.Name, f.Shared)
		}
	}
}

func TestEventsStream(t *testing.T) {
	a := start(t, "alice", "", nil)
	req, _ := http.NewRequest(http.MethodGet, a.srv.URL+"/ui/api/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	want := []string{"event: hello", "event: settings"}
	triggered := false
	for sc.Scan() {
		line := sc.Text()
		if line == want[0] {
			want = want[1:]
			if len(want) == 0 {
				return
			}
			if !triggered {
				triggered = true
				go func() {
					s := a.n.Settings()
					s.Name = "alice2"
					_ = a.n.SetSettings(context.Background(), s)
				}()
			}
		}
	}
	t.Fatalf("missing events: %v (%v)", want, sc.Err())
}

func TestLoadOrCreatePassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.password")
	p1, created, err := LoadOrCreatePassword(path)
	if err != nil || !created || len(p1) != 19 {
		t.Fatalf("creation: %q, %v, %v", p1, created, err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	p2, created, err := LoadOrCreatePassword(path)
	if err != nil || created || p2 != p1 {
		t.Fatalf("reading back: %q, %v, %v", p2, created, err)
	}
}
