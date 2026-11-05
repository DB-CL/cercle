// Package ui sert l'interface locale (côté LAN, jamais dans le tunnel) : une
// page unique embarquée et son API JSON sous /ui/api.
//
// Protection CSRF : toute requête qui modifie quelque chose doit porter
// l'en-tête X-Cercle-UI. Un autre site ne peut pas l'ajouter sans requête de
// contrôle préalable (CORS), à laquelle l'interface ne répond pas : il ne peut
// donc pas profiter du mot de passe mémorisé par le navigateur.
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DB-CL/cercle/internal/buildinfo"
	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/membership"
	"github.com/DB-CL/cercle/internal/node"
	"github.com/DB-CL/cercle/internal/transfer"
	"github.com/DB-CL/cercle/web"
)

// Server est l'interface locale d'un nœud.
type Server struct {
	Node     *node.Node
	Password string // vide = pas d'authentification (avertissement au démarrage)
	Logger   *slog.Logger

	authState authState
}

// Handler renvoie le routeur de l'interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/api/login", s.login)
	mux.HandleFunc("POST /ui/api/logout", s.logout)
	mux.HandleFunc("GET /ui/api/status", s.status)
	mux.HandleFunc("POST /ui/api/circle", s.createCircle)
	mux.HandleFunc("POST /ui/api/join", s.join)
	mux.HandleFunc("GET /ui/api/members", s.members)
	mux.HandleFunc("POST /ui/api/members/{id}/revoke", s.revoke)
	mux.HandleFunc("GET /ui/api/invites", s.invites)
	mux.HandleFunc("POST /ui/api/invites", s.createInvite)
	mux.HandleFunc("DELETE /ui/api/invites/{id}", s.cancelInvite)
	mux.HandleFunc("GET /ui/api/members/{id}/files", s.browse)
	mux.HandleFunc("GET /ui/api/queue", s.queue)
	mux.HandleFunc("POST /ui/api/queue", s.enqueue)
	mux.HandleFunc("POST /ui/api/queue/actions", s.queueActions)
	mux.HandleFunc("PATCH /ui/api/queue/{id}", s.queueAction)
	mux.HandleFunc("DELETE /ui/api/queue/{id}", s.queueRemove)
	mux.HandleFunc("GET /ui/api/settings", s.settings)
	mux.HandleFunc("PUT /ui/api/settings", s.putSettings)
	mux.HandleFunc("GET /ui/api/share", s.share)
	mux.HandleFunc("PUT /ui/api/share", s.putShare)
	mux.HandleFunc("POST /ui/api/scan", s.scan)
	mux.HandleFunc("GET /ui/api/events", s.eventsStream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	static, _ := fs.Sub(web.FS, ".")
	mux.Handle("GET /", http.FileServerFS(static))
	return s.headers(s.auth(s.csrf(mux)))
}

// headers ajoute les en-têtes de sécurité : pas de script externe, pas d'iframe.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// CSRFHeader doit accompagner toute requête qui modifie l'état.
const CSRFHeader = "X-Cercle-UI"

func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		default:
			if r.Header.Get(CSRFHeader) == "" {
				writeError(w, http.StatusForbidden, "en-tête "+CSRFHeader+" manquant")
				return
			}
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

func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	page, err := s.Node.BrowsePeer(ctx, r.PathValue("id"), r.URL.Query().Get("path"), r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

type queueResp struct {
	Summary transfer.Summary `json:"summary"`
	Items   []transfer.Item  `json:"items"`
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var statuses []transfer.Status
	for _, st := range strings.Split(q.Get("status"), ",") {
		if st = strings.TrimSpace(st); st != "" {
			statuses = append(statuses, transfer.Status(st))
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	sum, err := s.Node.Queue().Summarize(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	items, err := s.Node.Queue().ListPage(r.Context(), statuses, limit, offset)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, queueResp{Summary: sum, Items: items})
}

func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Peer  string   `json:"peer"`
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Peer == "" || len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "corps attendu : {\"peer\": \"<node_id>\", \"paths\": [\"…\"]}")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	added, skipped, err := s.Node.Queue().Enqueue(ctx, req.Peer, req.Paths)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added, "skipped": skipped})
}

// applyAction applique une action à un élément de la file.
func (s *Server) applyAction(ctx context.Context, id int64, action string, deletePart bool) error {
	q := s.Node.Queue()
	switch action {
	case "pause":
		return q.Pause(ctx, id)
	case "resume":
		return q.Resume(ctx, id)
	case "priority":
		return q.Prioritize(ctx, id)
	case "retry":
		return q.Retry(ctx, id)
	case "remove":
		return q.Remove(ctx, id, deletePart)
	}
	return fmt.Errorf("action inconnue %q (pause, resume, priority, retry, remove)", action)
}

func (s *Server) queueAction(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		Action string `json:"action"`
	}
	if err != nil || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		writeError(w, http.StatusBadRequest, "corps attendu : {\"action\": \"pause\"}")
		return
	}
	if err := s.applyAction(r.Context(), id, req.Action, false); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) queueRemove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "identifiant invalide")
		return
	}
	if err := s.Node.Queue().Remove(r.Context(), id, r.URL.Query().Get("part") == "1"); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// queueActions applique une action à une sélection, ou vide les terminés.
func (s *Server) queueActions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs        []int64 `json:"ids"`
		Action     string  `json:"action"`
		DeletePart bool    `json:"delete_part"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "corps illisible")
		return
	}
	if req.Action == "clear_done" {
		n, err := s.Node.Queue().ClearDone(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"removed": n})
		return
	}
	for _, id := range req.IDs {
		if err := s.applyAction(r.Context(), id, req.Action, req.DeletePart); err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"done": len(req.IDs)})
}

func (s *Server) settings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Node.Settings())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req node.Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "corps illisible")
		return
	}
	if err := s.Node.SetSettings(r.Context(), req); err != nil {
		var se node.SettingsError
		if errors.As(err, &se) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "réglages invalides", "fields": se})
			return
		}
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Node.Settings())
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	ix := s.Node.Index()
	size, files, last := ix.Totals(r.Context())
	_, limit, _, active := s.Node.Uploads().Limits()
	sel, err := ix.Selection(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	top, err := ix.TopLevel(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"size": size, "files": files, "last_scan": last, "dir": s.Node.Config().ShareDir,
		"uploads_active": active, "uploads_max": limit,
		"all": sel.All, "folders": top,
	})
}

// putShare choisit ce qui est partagé au premier niveau de /share, puis relance l'indexation.
func (s *Server) putShare(w http.ResponseWriter, r *http.Request) {
	var sel index.Selection
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&sel); err != nil {
		writeError(w, http.StatusBadRequest, "corps attendu : {\"all\": false, \"folders\": [\"films\"]}")
		return
	}
	if err := s.Node.Index().SetSelection(r.Context(), sel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.share(w, r)
}

func (s *Server) scan(w http.ResponseWriter, _ *http.Request) {
	s.Node.ScanNow()
	w.WriteHeader(http.StatusAccepted)
}

// eventsStream envoie en Server-Sent Events les notifications de changement
// (« queue », « members », « settings ») et, chaque seconde pendant un
// transfert, la progression des éléments actifs.
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "flux non pris en charge")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := s.Node.Events().Subscribe()
	defer cancel()
	send := func(event string, data any) bool {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send("hello", map[string]string{"version": buildinfo.Version}) {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	pending := map[string]bool{}
	for {
		select {
		case <-r.Context().Done():
			return
		case kind := <-ch:
			pending[kind] = true // regroupés, envoyés au prochain tic
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-tick.C:
			for kind := range pending {
				if !send(kind, map[string]string{}) {
					return
				}
			}
			clear(pending)
			if active, err := s.Node.Queue().Active(r.Context()); err == nil && len(active) > 0 {
				if !send("progress", active) {
					return
				}
			}
		}
	}
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
