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

	// Caps on what we accept from a peer: a malicious member must not be able
	// to exhaust another's memory with an endless response.
	maxResponse = 8 << 20   // gossip table, folder page, info, join
	maxTree     = 512 << 20 // NDJSON tree (500,000 files ≈ 75 MB)
)

// DialFunc opens a TCP connection in the tunnel.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Client calls the other nodes' API.
type Client struct {
	http     *http.Client
	circleID string
}

// NewClient creates a client going through the tunnel. Timeouts: 10 s to
// connect, 60 s without any byte exchanged before giving up.
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

// WithCircle returns a client sharing c's connections but presenting another
// circle ID (the circle is known after joining).
func (c *Client) WithCircle(circle string) *Client {
	return &Client{http: c.http, circleID: circle}
}

// StatusError is a non-2xx HTTP response.
type StatusError struct {
	Code       int
	Message    string
	RetryAfter time.Duration // 0 = absent
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("peer replied %d: %s", e.Code, e.Message)
}

// HTTPStatus returns the HTTP code (interface read by transfer, without a dependency).
func (e *StatusError) HTTPStatus() int { return e.Code }

// RetryDelay returns the peer's Retry-After, 0 if absent.
func (e *StatusError) RetryDelay() time.Duration { return e.RetryAfter }

// Download requests the remote file from offset. If offset > 0, resuming is
// conditioned on etag (If-Range): if the file changed, the peer returns 200
// and the whole file. The caller must close the body.
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

// Info queries GET /api/v1/info.
func (c *Client) Info(ctx context.Context, peer netip.Addr) (Info, error) {
	var info Info
	err := c.getJSON(ctx, peer, "/api/v1/info", &info)
	return info, err
}

// Join sends a join request (with the temporary key). Known refusals are
// returned as membership errors.
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
		case http.StatusUnprocessableEntity:
			return resp, membership.ErrInviteAddressTaken
		}
	}
	return resp, err
}

// Gossip sends the local table and returns the peer's after merging.
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
		return fmt.Errorf("unreadable response from %v (%s): %w", peer, path, err)
	}
	return nil
}

// Files reads a page of a remote folder ("" = root).
func (c *Client) Files(ctx context.Context, peer netip.Addr, dir, cursor string) (FilesPage, error) {
	q := url.Values{"path": {dir}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var page FilesPage
	err := c.getJSON(ctx, peer, "/api/v1/files?"+q.Encode(), &page)
	return page, err
}

// Tree calls fn for every remote file under dir.
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
			return fmt.Errorf("unreadable tree from %v: %w", peer, err)
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
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set(CircleHeader, c.circleID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("peer %v unreachable: %w", peer, err)
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
		return fmt.Errorf("unreadable response from %v (%s): %w", peer, path, err)
	}
	return nil
}

// idleConn closes the connection after 60 s without any byte read or
// written: a vanishing peer never blocks a transfer.
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
