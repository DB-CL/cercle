// Package peerapi is the node-to-node HTTP API. It only listens inside the
// WireGuard tunnel: encryption and authentication come from WireGuard, and
// the caller's identity is its source address in the tunnel.
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
	"strconv"
	"time"

	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/membership"
	"github.com/DB-CL/cercle/internal/transfer"
)

// Port is the API TCP port, on the tunnel address.
const Port = 8787

// CircleHeader carries the circle ID in every request.
const CircleHeader = "X-Cercle-Circle"

// MtimeHeader gives the exact modification time (Unix nanoseconds) of a
// served file: Last-Modified only has one-second precision.
const MtimeHeader = transfer.MtimeHeader

// Member is a known member, identified by its tunnel address.
type Member struct {
	NodeID   string // WireGuard public key in base64
	Name     string
	TunnelIP netip.Addr
}

// Directory finds the (non-revoked) member owning a tunnel address.
type Directory interface {
	MemberByIP(ip netip.Addr) (Member, bool)
}

// Info is the response of GET /api/v1/info.
type Info struct {
	Name       string     `json:"name"`
	Version    string     `json:"version"`
	ShareSize  int64      `json:"share_size"`
	ShareFiles int64      `json:"share_files"`
	LastScan   *time.Time `json:"last_scan,omitempty"`
}

// Server serves the peer API.
type Server struct {
	CircleID  string
	Directory Directory
	Info      func(ctx context.Context) Info
	Index     *index.Index
	Uploads   *transfer.Uploads
	Logger    *slog.Logger

	// Membership. InviteIP reports whether an address belongs to a temporary
	// invitation key: it can only reach POST /api/v1/join.
	InviteIP func(ctx context.Context, ip netip.Addr) bool
	Join     func(ctx context.Context, tempIP netip.Addr, req membership.JoinRequest) (membership.JoinResponse, error)
	Gossip   func(ctx context.Context, from Member, t membership.Table) (membership.Table, error)
}

// FilesPage is the response of GET /api/v1/files.
type FilesPage struct {
	Entries []index.Entry `json:"entries"`
	Next    string        `json:"next,omitempty"`
}

type ctxKey struct{}

// MemberFrom returns the authenticated member of the request.
func MemberFrom(ctx context.Context) (Member, bool) {
	m, ok := ctx.Value(ctxKey{}).(Member)
	return m, ok
}

// Handler returns the router, behind the identity middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/info", s.handleInfo)
	mux.HandleFunc("GET /api/v1/files", s.handleFiles)
	mux.HandleFunc("GET /api/v1/tree", s.handleTree)
	mux.HandleFunc("GET /api/v1/file", s.handleFile)
	mux.HandleFunc("POST /api/v1/gossip", s.handleGossip)
	join := http.HandlerFunc(s.handleJoin)
	return s.identify(mux, join)
}

// identify ties every request to a member through its source IP,
// authenticated by WireGuard. A temporary invitation key only reaches join.
// Unknown address or other circle: 403.
func (s *Server) identify(next, join http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			writeError(w, http.StatusForbidden, "unreadable source address")
			return
		}
		if s.InviteIP != nil && s.InviteIP(r.Context(), ap.Addr().Unmap()) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/join" || s.Join == nil {
				writeError(w, http.StatusForbidden, "an invitation key can only reach /api/v1/join")
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(CircleHeader)), []byte(s.CircleID)) != 1 {
				writeError(w, http.StatusForbidden, "unknown circle")
				return
			}
			join.ServeHTTP(w, r)
			return
		}
		m, ok := s.Directory.MemberByIP(ap.Addr().Unmap())
		if !ok {
			s.log().Warn("request from an unknown address refused", "ip", ap.Addr(), "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "unknown tunnel address")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(CircleHeader)), []byte(s.CircleID)) != 1 {
			s.log().Warn("request from another circle refused", "member", m.Name, "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "unknown circle")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, m)))
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Info(r.Context()))
}

const maxTableBytes = 4 << 20

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	ap, _ := netip.ParseAddrPort(r.RemoteAddr)
	var req membership.JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "unreadable join request")
		return
	}
	resp, err := s.Join(r.Context(), ap.Addr().Unmap(), req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, resp)
	case errors.Is(err, membership.ErrInviteUsed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, membership.ErrInviteExpired):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, membership.ErrInviteUnknown):
		writeError(w, http.StatusForbidden, err.Error())
	default:
		s.log().Warn("join refused", "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func (s *Server) handleGossip(w http.ResponseWriter, r *http.Request) {
	if s.Gossip == nil {
		writeError(w, http.StatusNotFound, "gossip unavailable")
		return
	}
	m, _ := MemberFrom(r.Context())
	var t membership.Table
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTableBytes)).Decode(&t); err != nil {
		writeError(w, http.StatusBadRequest, "unreadable table")
		return
	}
	out, err := s.Gossip(r.Context(), m, t)
	if err != nil {
		s.log().Error("gossip", "member", m.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// sharePath reads and validates the path parameter; on refusal the response is already written.
func (s *Server) sharePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, err := index.CleanPath(r.URL.Query().Get("path"))
	if err != nil {
		m, _ := MemberFrom(r.Context())
		s.log().Warn("path refused", "member", m.Name, "path", r.URL.Query().Get("path"))
		writeError(w, http.StatusBadRequest, "path refused")
		return "", false
	}
	return p, true
}

// indexError translates an index error into an HTTP response.
func (s *Server) indexError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, index.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, index.ErrBadPath):
		writeError(w, http.StatusBadRequest, "path refused")
	default:
		s.log().Error("index error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := s.sharePath(w, r)
	if !ok {
		return
	}
	entries, next, err := s.Index.List(r.Context(), p, r.URL.Query().Get("cursor"), index.PageSize)
	if err != nil {
		s.indexError(w, err)
		return
	}
	if entries == nil {
		entries = []index.Entry{}
	}
	writeJSON(w, http.StatusOK, FilesPage{Entries: entries, Next: next})
}

// handleTree returns, as NDJSON, every file under a path.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	p, ok := s.sharePath(w, r)
	if !ok {
		return
	}
	if _, err := s.Index.Lookup(r.Context(), p); err != nil {
		s.indexError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	err := s.Index.Tree(r.Context(), p, func(e index.Entry) error { return enc.Encode(e) })
	if err != nil {
		// Headers are already sent: abort, the client will see a truncated stream.
		s.log().Warn("tree interrupted", "path", p, "err", err)
		panic(http.ErrAbortHandler)
	}
}

// handleFile serves a file with Range and If-Range, throttled by the upload limits.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.sharePath(w, r)
	if !ok {
		return
	}
	release, retry, ok := s.Uploads.Acquire()
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
		writeError(w, http.StatusServiceUnavailable, "uploads unavailable, retry later")
		return
	}
	defer release()
	f, info, err := s.Index.Open(r.Context(), p)
	if err != nil {
		s.indexError(w, err)
		return
	}
	defer f.Close()
	m, _ := MemberFrom(r.Context())
	s.log().Info("sending", "member", m.Name, "path", p, "range", r.Header.Get("Range"))
	w.Header().Set("ETag", ETag(info.Size(), info.ModTime()))
	w.Header().Set(MtimeHeader, strconv.FormatInt(info.ModTime().UnixNano(), 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), s.Uploads.Reader(r.Context(), f))
}

// ETag identifies a file version: size and time in nanoseconds.
func ETag(size int64, mtime time.Time) string {
	return `"` + strconv.FormatInt(size, 10) + "-" + strconv.FormatInt(mtime.UnixNano(), 10) + `"`
}

// Serve serves the API on the tunnel listener until ctx is cancelled, then
// gives in-flight requests 10 s.
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
