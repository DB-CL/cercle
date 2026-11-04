// Package peerapi est l'API HTTP entre nœuds. Elle n'écoute que dans le
// tunnel WireGuard : le chiffrement et l'authentification viennent de lui, et
// l'identité de l'appelant est son adresse source dans le tunnel.
package peerapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Port est le port TCP de l'API, sur l'adresse de tunnel.
const Port = 8787

// CircleHeader porte l'identifiant du cercle dans chaque requête.
const CircleHeader = "X-Cercle-Circle"

// Member est un membre connu, identifié par son adresse de tunnel.
type Member struct {
	NodeID   string // clé publique WireGuard en base64
	Name     string
	TunnelIP netip.Addr
}

// Directory retrouve le membre (non révoqué) qui possède une adresse de tunnel.
type Directory interface {
	MemberByIP(ip netip.Addr) (Member, bool)
}

// Info est la réponse de GET /api/v1/info.
type Info struct {
	Name       string     `json:"name"`
	Version    string     `json:"version"`
	ShareSize  int64      `json:"share_size"`
	ShareFiles int64      `json:"share_files"`
	LastScan   *time.Time `json:"last_scan,omitempty"`
}

// Server sert l'API pair-à-pair.
type Server struct {
	CircleID  string
	Directory Directory
	Info      func(ctx context.Context) Info
	Logger    *slog.Logger
}

type ctxKey struct{}

// MemberFrom renvoie le membre authentifié de la requête.
func MemberFrom(ctx context.Context) (Member, bool) {
	m, ok := ctx.Value(ctxKey{}).(Member)
	return m, ok
}

// Handler renvoie le routeur, derrière le middleware d'identité.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/info", s.handleInfo)
	return s.identify(mux)
}

// identify rattache chaque requête à un membre par son IP source, authentifiée
// par WireGuard. Adresse inconnue ou cercle différent : 403.
func (s *Server) identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			writeError(w, http.StatusForbidden, "adresse source illisible")
			return
		}
		m, ok := s.Directory.MemberByIP(ap.Addr().Unmap())
		if !ok {
			s.log().Warn("requête d'une adresse inconnue refusée", "ip", ap.Addr(), "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "adresse de tunnel inconnue")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(CircleHeader)), []byte(s.CircleID)) != 1 {
			s.log().Warn("requête d'un autre cercle refusée", "member", m.Name, "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "cercle inconnu")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, m)))
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Info(r.Context()))
}

// Serve sert l'API sur l'écouteur du tunnel jusqu'à l'annulation de ctx, puis
// laisse 10 s aux requêtes en cours.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log().Handler(), slog.LevelDebug),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutCtx)
	if e := <-errc; !errors.Is(e, http.ErrServerClosed) && err == nil {
		err = e
	}
	return err
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default().With("module", "peerapi")
	}
	return s.Logger
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
