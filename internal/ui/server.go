// Package ui sert l'interface locale (côté LAN, jamais dans le tunnel).
//
// J4 en livre la partie adhésion de l'API (/ui/api/status, circle, join,
// invites, members) ; la page web et le reste de l'API arrivent en J5.
package ui

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/DB-CL/cercle/internal/buildinfo"
	"github.com/DB-CL/cercle/internal/membership"
	"github.com/DB-CL/cercle/internal/node"
)

// Server est l'interface locale d'un nœud.
type Server struct {
	Node     *node.Node
	Password string // vide = pas d'authentification (avertissement au démarrage)
	Logger   *slog.Logger
}

// Handler renvoie le routeur de l'interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/api/status", s.status)
	mux.HandleFunc("POST /ui/api/circle", s.createCircle)
	mux.HandleFunc("POST /ui/api/join", s.join)
	mux.HandleFunc("GET /ui/api/members", s.members)
	mux.HandleFunc("POST /ui/api/members/{id}/revoke", s.revoke)
	mux.HandleFunc("GET /ui/api/invites", s.invites)
	mux.HandleFunc("POST /ui/api/invites", s.createInvite)
	mux.HandleFunc("DELETE /ui/api/invites/{id}", s.cancelInvite)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return s.auth(mux)
}

// auth applique l'authentification Basic si un mot de passe est défini.
// /healthz reste ouvert pour le healthcheck du conteneur.
func (s *Server) auth(next http.Handler) http.Handler {
	if s.Password == "" {
		return next
	}
	want := sha256.Sum256([]byte(s.Password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		_, pass, ok := r.BasicAuth()
		got := sha256.Sum256([]byte(pass))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Cercle", charset="UTF-8"`)
			writeError(w, http.StatusUnauthorized, "mot de passe requis")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Serve sert l'interface sur addr jusqu'à l'annulation de ctx.
func (s *Server) Serve(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	<-errc
	return nil
}

type statusResp struct {
	Version   string             `json:"version"`
	InCircle  bool               `json:"in_circle"`
	CircleID  string             `json:"circle_id,omitempty"`
	Subnet    string             `json:"subnet,omitempty"`
	Self      *membership.Member `json:"self,omitempty"`
	PublicKey string             `json:"public_key"`
	TunnelUp  bool               `json:"tunnel_up"`
	Endpoints []string           `json:"announced_endpoints"`
	ListenUDP int                `json:"listen_port"`
	Password  bool               `json:"password_set"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	n := s.Node
	cfg := n.Config()
	resp := statusResp{Version: buildinfo.Version, InCircle: n.Membership().InCircle(), PublicKey: n.PublicKey().String(),
		TunnelUp: n.TunnelUp(), Endpoints: cfg.WGPublicEndpoints, ListenUDP: cfg.WGListenPort, Password: s.Password != ""}
	if resp.InCircle {
		id, subnet := n.Membership().Circle()
		resp.CircleID, resp.Subnet = id, subnet.String()
		if self, ok := n.Membership().Self(); ok {
			resp.Self = &self
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) createCircle(w http.ResponseWriter, r *http.Request) {
	if err := s.Node.CreateCircle(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.status(w, r)
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "corps illisible : {\"code\": \"cercle1:…\"} attendu")
		return
	}
	if err := s.Node.Join(r.Context(), req.Code); err != nil {
		s.fail(w, err)
		return
	}
	s.status(w, r)
}

func (s *Server) members(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Node.Peers())
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if err := s.Node.Revoke(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Node.Peers())
}

func (s *Server) invites(w http.ResponseWriter, r *http.Request) {
	invs, err := s.Node.Membership().Invites(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if invs == nil {
		invs = []membership.Invite{}
	}
	writeJSON(w, http.StatusOK, invs)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	code, inv, err := s.Node.Invite(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	// Le code n'est renvoyé qu'ici, à sa création : il n'est stocké nulle part.
	writeJSON(w, http.StatusCreated, struct {
		membership.Invite
		Code string `json:"code"`
	}{inv, code})
}

func (s *Server) cancelInvite(w http.ResponseWriter, r *http.Request) {
	if err := s.Node.Membership().CancelInvite(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fail traduit une erreur en réponse, avec un message lisible par l'utilisateur.
func (s *Server) fail(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, membership.ErrInviteUsed), errors.Is(err, membership.ErrAlreadyInCircle):
		code = http.StatusConflict
	case errors.Is(err, membership.ErrInviteExpired):
		code = http.StatusGone
	case errors.Is(err, membership.ErrNotInCircle):
		code = http.StatusPreconditionFailed
	}
	if s.Logger != nil {
		s.Logger.Info("action refusée", "err", err)
	}
	writeError(w, code, err.Error())
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
