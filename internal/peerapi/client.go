package peerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/membership"
)

const (
	connectTimeout = 10 * time.Second
	idleTimeout    = 60 * time.Second

	// Plafonds de ce qu'on accepte d'un pair : un membre malveillant ne doit
	// pas pouvoir épuiser la mémoire d'un autre avec une réponse sans fin.
	maxResponse = 8 << 20   // table de gossip, page de dossier, info, adhésion
	maxTree     = 512 << 20 // arborescence en NDJSON (500 000 fichiers ≈ 75 Mo)
)

// DialFunc ouvre une connexion TCP dans le tunnel.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Client appelle l'API des autres nœuds.
type Client struct {
	http     *http.Client
	circleID string
}

// NewClient crée un client qui passe par le tunnel. Délais : 10 s pour se
// connecter, 60 s sans aucun octet échangé avant d'abandonner.
func NewClient(dial DialFunc, circleID string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			c, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &idleConn{Conn: c}, nil
		},
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: idleTimeout,
		DisableCompression:    true,
	}
	return &Client{http: &http.Client{Transport: tr}, circleID: circleID}
}

// WithCircle renvoie un client qui partage les connexions de c mais présente
// un autre identifiant de cercle (le cercle est connu après l'adhésion).
func (c *Client) WithCircle(circle string) *Client {
	return &Client{http: c.http, circleID: circle}
}

// StatusError est une réponse HTTP autre que 2xx.
type StatusError struct {
	Code       int
	Message    string
	RetryAfter time.Duration // 0 = absent
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("le pair a répondu %d : %s", e.Code, e.Message)
}

// HTTPStatus renvoie le code HTTP (interface lue par transfer, sans dépendance).
func (e *StatusError) HTTPStatus() int { return e.Code }

// RetryDelay renvoie le Retry-After du pair, 0 s'il est absent.
func (e *StatusError) RetryDelay() time.Duration { return e.RetryAfter }

// Download demande le fichier distant à partir de offset. Si offset > 0, la
// reprise est conditionnée par etag (If-Range) : si le fichier a changé, le
// pair renvoie 200 et le fichier entier. Le corps est à fermer par l'appelant.
func (c *Client) Download(ctx context.Context, peer netip.Addr, path string, offset int64, etag string) (*http.Response, error) {
	hdr := http.Header{}
	if offset > 0 {
		hdr.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		if etag != "" {
			hdr.Set("If-Range", etag)
		}
	}
	return c.doHeader(ctx, http.MethodGet, peer, "/api/v1/file?"+url.Values{"path": {path}}.Encode(), nil, hdr)
}

// Info interroge GET /api/v1/info.
func (c *Client) Info(ctx context.Context, peer netip.Addr) (Info, error) {
	var info Info
	err := c.getJSON(ctx, peer, "/api/v1/info", &info)
	return info, err
}

// Join envoie une demande d'adhésion (avec la clé temporaire). Les refus
// connus sont rendus sous forme d'erreurs de membership.
func (c *Client) Join(ctx context.Context, inviter netip.Addr, req membership.JoinRequest) (membership.JoinResponse, error) {
	var resp membership.JoinResponse
	err := c.postJSON(ctx, inviter, "/api/v1/join", req, &resp)
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusConflict:
			return resp, membership.ErrInviteUsed
		case http.StatusGone:
			return resp, membership.ErrInviteExpired
		case http.StatusForbidden:
			return resp, membership.ErrInviteUnknown
		}
	}
	return resp, err
}

// Gossip envoie la table locale et renvoie celle du pair après fusion.
func (c *Client) Gossip(ctx context.Context, peer netip.Addr, t membership.Table) (membership.Table, error) {
	var out membership.Table
	err := c.postJSON(ctx, peer, "/api/v1/gossip", t, &out)
	return out, err
}

func (c *Client) postJSON(ctx context.Context, peer netip.Addr, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.doHeader(ctx, http.MethodPost, peer, path, bytes.NewReader(b), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
		return fmt.Errorf("réponse de %v illisible (%s) : %w", peer, path, err)
	}
	return nil
}

// Files lit une page du contenu d'un dossier distant ("" = racine).
func (c *Client) Files(ctx context.Context, peer netip.Addr, dir, cursor string) (FilesPage, error) {
	q := url.Values{"path": {dir}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var page FilesPage
	err := c.getJSON(ctx, peer, "/api/v1/files?"+q.Encode(), &page)
	return page, err
}

// Tree appelle fn pour chaque fichier distant sous dir.
func (c *Client) Tree(ctx context.Context, peer netip.Addr, dir string, fn func(index.Entry) error) error {
	resp, err := c.do(ctx, http.MethodGet, peer, "/api/v1/tree?"+url.Values{"path": {dir}}.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxTree))
	for {
		var e index.Entry
		err := dec.Decode(&e)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("arborescence de %v illisible : %w", peer, err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
}

func (c *Client) do(ctx context.Context, method string, peer netip.Addr, path string, body io.Reader) (*http.Response, error) {
	return c.doHeader(ctx, method, peer, path, body, nil)
}

func (c *Client) doHeader(ctx context.Context, method string, peer netip.Addr, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	target := "http://" + netip.AddrPortFrom(peer, Port).String() + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("requête %s : %w", path, err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set(CircleHeader, c.circleID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pair %v injoignable : %w", peer, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		se := &StatusError{Code: resp.StatusCode}
		var eb errorBody
		if json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&eb) == nil {
			se.Message = eb.Error
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			se.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, se
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, peer netip.Addr, path string, v any) error {
	resp, err := c.do(ctx, http.MethodGet, peer, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(v); err != nil {
		return fmt.Errorf("réponse de %v illisible (%s) : %w", peer, path, err)
	}
	return nil
}

// idleConn coupe la connexion après 60 s sans aucun octet lu ou écrit : un
// pair qui disparaît ne bloque jamais un transfert.
type idleConn struct {
	net.Conn
}

func (c *idleConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(idleTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}
