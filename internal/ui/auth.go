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
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Connexion à l'interface : un mot de passe (UI_PASSWORD) saisi dans la page,
// puis un cookie de session signé.
//
// Le cookie vaut « <expiration>.<signature> », signé en HMAC-SHA256 avec une
// clé dérivée d'un secret propre au nœud (stocké en base) ET du mot de passe :
// changer UI_PASSWORD invalide toutes les sessions. Rien n'est stocké par
// session. HttpOnly, SameSite=Strict, Secure derrière HTTPS.
//
// L'authentification Basic reste acceptée pour les scripts (curl), mais
// l'interface n'envoie jamais WWW-Authenticate : le navigateur n'affiche plus
// sa fenêtre de connexion.

const (
	sessionCookie   = "cercle_session"
	sessionRemember = 30 * 24 * time.Hour // « Rester connecté »
	sessionDefault  = 12 * time.Hour      // sinon : cookie de session du navigateur
	settingSecret   = "ui.session_secret"
)

type authState struct {
	once     sync.Once
	key      []byte     // clé de signature des sessions
	loginMu  sync.Mutex // un essai à la fois : la recherche par force brute est sérialisée
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
				s.Logger.Error("enregistrement du secret de session", "err", err)
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

// public dit si un chemin est accessible sans être connecté : la page et ses
// fichiers (sans secret), la connexion elle-même et le healthcheck.
func public(path string) bool {
	if !strings.HasPrefix(path, "/ui/api/") {
		return true
	}
	return path == "/ui/api/login" || path == "/ui/api/logout"
}

// auth protège l'API si un mot de passe est défini.
func (s *Server) auth(next http.Handler) http.Handler {
	if s.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r.URL.Path) || s.validSession(r) || s.validBasic(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "connexion requise", "login": true})
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
		writeError(w, http.StatusBadRequest, "demande illisible")
		return
	}
	s.sessionKey()
	s.authState.loginMu.Lock()
	defer s.authState.loginMu.Unlock()
	got, want := sha256.Sum256([]byte(req.Password)), sha256.Sum256([]byte(s.Password))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		if s.Logger != nil {
			s.Logger.Warn("mot de passe de l'interface incorrect", "remote", r.RemoteAddr, "forwarded_for", r.Header.Get("X-Forwarded-For"))
		}
		time.Sleep(s.authState.failWait) // au plus un essai par seconde
		writeError(w, http.StatusUnauthorized, "Mot de passe incorrect.")
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
