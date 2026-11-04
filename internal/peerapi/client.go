package peerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

const (
	connectTimeout = 10 * time.Second
	idleTimeout    = 60 * time.Second
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

// StatusError est une réponse HTTP autre que 2xx.
type StatusError struct {
	Code       int
	Message    string
	RetryAfter time.Duration // 0 = absent
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("le pair a répondu %d : %s", e.Code, e.Message)
}

// Info interroge GET /api/v1/info.
func (c *Client) Info(ctx context.Context, peer netip.Addr) (Info, error) {
	var info Info
	err := c.getJSON(ctx, peer, "/api/v1/info", &info)
	return info, err
}

func (c *Client) do(ctx context.Context, method string, peer netip.Addr, path string, body io.Reader) (*http.Response, error) {
	url := "http://" + netip.AddrPortFrom(peer, Port).String() + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("requête %s : %w", path, err)
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
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
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
