package ui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Interface login: a password (UI_PASSWORD) typed in the page, then a signed
// session cookie.
//
// The cookie is "<expiry>.<signature>", signed with HMAC-SHA256 using a key
// derived from a node-specific secret (stored in the database) AND the
// password: changing UI_PASSWORD invalidates every session. Nothing is stored
// per session. HttpOnly, SameSite=Strict, Secure behind HTTPS.
//
// Basic authentication is still accepted for scripts (curl), but the
// interface never sends WWW-Authenticate: the browser no longer shows its
// login dialog.

const (
	sessionCookie   = "cercle_session"
	sessionRemember = 30 * 24 * time.Hour // "Stay signed in"
	sessionDefault  = 12 * time.Hour      // otherwise: browser session cookie
	settingSecret   = "ui.session_secret"
)

type authState struct {
	once     sync.Once
	key      []byte     // session signing key
	loginMu  sync.Mutex // one attempt at a time: brute force is serialized
	failWait time.Duration
}

func (s *Server) sessionKey() []byte {
	s.authState.once.Do(func() {
		ctx := context.Background()
		st := s.Node.Store()
		secret, ok, err := st.Setting(ctx, settingSecret)
		if err != nil || !ok {
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			secret = hex.EncodeToString(b)
			if err := st.SetSetting(ctx, settingSecret, secret); err != nil && s.Logger != nil {
				s.Logger.Error("storing the session secret", "err", err)
			}
		}
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte("cercle1/session\x00" + s.Password))
		s.authState.key = m.Sum(nil)
		if s.authState.failWait == 0 {
			s.authState.failWait = time.Second
		}
	})
	return s.authState.key
}

func (s *Server) sign(exp int64) string {
	m := hmac.New(sha256.New, s.sessionKey())
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	expS, _, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(expS, 10, 64)
	if !ok || err != nil || time.Now().Unix() >= exp {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.sign(exp))) == 1
}

func (s *Server) validBasic(r *http.Request) bool {
	_, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	got, want := sha256.Sum256([]byte(pass)), sha256.Sum256([]byte(s.Password))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// public reports whether a path is reachable without being signed in: the
// page and its files (no secrets), the login itself and the healthcheck.
func public(path string) bool {
	if !strings.HasPrefix(path, "/ui/api/") {
		return true
	}
	return path == "/ui/api/login" || path == "/ui/api/logout"
}

// auth protects the API when a password is set.
func (s *Server) auth(next http.Handler) http.Handler {
	if s.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r.URL.Path) || s.validSession(r) || s.validBasic(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "login required", "login": true})
	})
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.Password == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req struct {
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "unreadable request")
		return
	}
	s.sessionKey()
	s.authState.loginMu.Lock()
	defer s.authState.loginMu.Unlock()
	got, want := sha256.Sum256([]byte(req.Password)), sha256.Sum256([]byte(s.Password))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		if s.Logger != nil {
			s.Logger.Warn("wrong interface password", "remote", r.RemoteAddr, "forwarded_for", r.Header.Get("X-Forwarded-For"))
		}
		time.Sleep(s.authState.failWait) // at most one attempt per second
		writeError(w, http.StatusUnauthorized, "Wrong password.")
		return
	}
	life := sessionDefault
	if req.Remember {
		life = sessionRemember
	}
	c := &http.Cookie{
		Name: sessionCookie, Value: s.sign(time.Now().Add(life).Unix()), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureRequest(r),
	}
	if req.Remember {
		c.MaxAge = int(life / time.Second)
	}
	http.SetCookie(w, c)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureRequest(r)})
	w.WriteHeader(http.StatusNoContent)
}

// LoadOrCreatePassword returns the interface password stored in path, or
// creates a random one (mode 0600) if there is none. Used when UI_PASSWORD is
// empty: the interface is never open without a password, since anyone on the
// local network (or a web page, through DNS rebinding) could otherwise invite
// themselves into the circle.
func LoadOrCreatePassword(path string) (pass string, created bool, err error) {
	if b, err := os.ReadFile(path); err == nil {
		if p := strings.TrimSpace(string(b)); p != "" {
			return p, false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no 0, o, 1, l
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	var sb strings.Builder
	for i, b := range raw {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	pass = sb.String()
	if err := os.WriteFile(path, []byte(pass+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("writing %s: %w", path, err)
	}
	return pass, true, nil
}
