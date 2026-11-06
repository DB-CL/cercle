package node

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/membership"
	"github.com/DB-CL/cercle/internal/peerapi"
)

// Hub broadcasts change notifications ("queue", "members") to interface
// clients. A slow subscriber loses notifications, never state: it reads back
// what changed.
type Hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

// Subscribe subscribes; cancel unsubscribes.
func (h *Hub) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 16)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan string]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Publish notifies every subscriber.
func (h *Hub) Publish(kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- kind:
		default:
		}
	}
}

// Events returns the notification hub.
func (n *Node) Events() *Hub { return &n.events }

// Settings are the settings editable from the interface. Rates use the
// environment format ("20M", "0" = unlimited), windows "HH:MM-HH:MM" or empty.
type Settings struct {
	Name                string `json:"name"`
	UploadLimit         string `json:"upload_limit"`
	MaxUploads          int    `json:"max_uploads"`
	UploadWindow        string `json:"upload_window"`
	DownloadLimit       string `json:"download_limit"`
	DownloadConcurrency int    `json:"download_concurrency"`
	DownloadWindow      string `json:"download_window"`
}

func windowString(w *config.Window) string {
	if w == nil {
		return ""
	}
	return w.String()
}

// Settings returns the settings in effect.
func (n *Node) Settings() Settings {
	c := n.Config()
	return Settings{
		Name: c.NodeName, UploadLimit: config.FormatRate(c.UploadLimit), MaxUploads: c.MaxUploads,
		UploadWindow: windowString(c.UploadWindow), DownloadLimit: config.FormatRate(c.DownloadLimit),
		DownloadConcurrency: c.DownloadConcurrency, DownloadWindow: windowString(c.DownloadWindow),
	}
}

// SettingsError reports invalid settings, field by field.
type SettingsError map[string]string

func (e SettingsError) Error() string {
	var parts []string
	for k, v := range e {
		parts = append(parts, k+": "+v)
	}
	return "invalid settings (" + strings.Join(parts, "; ") + ")"
}

// SetSettings validates, stores and applies new settings live. Once stored,
// they take precedence over the environment variables.
func (n *Node) SetSettings(ctx context.Context, s Settings) error {
	errs := SettingsError{}
	name := strings.TrimSpace(s.Name)
	if name == "" || len(name) > 64 {
		errs["name"] = "expected a name of 1 to 64 characters"
	}
	up, err := config.ParseRate(s.UploadLimit)
	if err != nil {
		errs["upload_limit"] = err.Error()
	}
	down, err := config.ParseRate(s.DownloadLimit)
	if err != nil {
		errs["download_limit"] = err.Error()
	}
	if s.MaxUploads < 1 || s.MaxUploads > 50 {
		errs["max_uploads"] = "between 1 and 50"
	}
	if s.DownloadConcurrency < 1 || s.DownloadConcurrency > 20 {
		errs["download_concurrency"] = "between 1 and 20"
	}
	parseWin := func(field, v string) *config.Window {
		if strings.TrimSpace(v) == "" {
			return nil
		}
		w, err := config.ParseWindow(v)
		if err != nil {
			errs[field] = err.Error()
			return nil
		}
		return &w
	}
	upWin := parseWin("upload_window", s.UploadWindow)
	downWin := parseWin("download_window", s.DownloadWindow)
	if len(errs) > 0 {
		return errs
	}

	values := map[string]string{
		config.SettingNodeName: name, config.SettingUploadLimit: config.FormatRate(up),
		config.SettingMaxUploads: strconv.Itoa(s.MaxUploads), config.SettingUploadWindow: windowString(upWin),
		config.SettingDownloadLimit: config.FormatRate(down), config.SettingDownloadConcurrency: strconv.Itoa(s.DownloadConcurrency),
		config.SettingDownloadWindow: windowString(downWin),
	}
	for k, v := range values {
		if err := n.st.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	n.cfgMu.Lock()
	n.cfg.NodeName, n.cfg.UploadLimit, n.cfg.MaxUploads, n.cfg.UploadWindow = name, up, s.MaxUploads, upWin
	n.cfg.DownloadLimit, n.cfg.DownloadConcurrency, n.cfg.DownloadWindow = down, s.DownloadConcurrency, downWin
	n.cfgMu.Unlock()
	n.up.SetLimit(up)
	n.up.SetMax(s.MaxUploads)
	n.up.SetWindow(upWin)
	n.queue.SetLimit(down)
	n.queue.SetConcurrency(s.DownloadConcurrency)
	n.queue.SetWindow(downWin)
	if n.ms.InCircle() {
		changed, err := n.ms.UpdateSelf(ctx, func(b *membership.Body) { b.Name = name })
		if err != nil {
			return err
		}
		if changed {
			n.poke()
		}
	}
	n.events.Publish("settings")
	return nil
}

// Config returns a copy of the configuration in effect.
func (n *Node) Config() config.Config {
	n.cfgMu.Lock()
	defer n.cfgMu.Unlock()
	return n.cfg
}

type peerInfo struct {
	info peerapi.Info
	at   time.Time
}

// refreshInfo queries online peers whose info is older than a minute.
func (n *Node) refreshInfo(ctx context.Context) {
	var g sync.WaitGroup
	for _, p := range n.Peers() {
		if p.Self || p.Revoked || !p.Online {
			continue
		}
		n.infoMu.Lock()
		old := n.infos[p.NodeID]
		n.infoMu.Unlock()
		if time.Since(old.at) < time.Minute {
			continue
		}
		g.Go(func() {
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			info, err := n.peerClient().Info(rctx, p.TunnelIP)
			if err != nil {
				return
			}
			n.infoMu.Lock()
			n.infos[p.NodeID] = peerInfo{info: info, at: time.Now()}
			n.infoMu.Unlock()
			n.events.Publish("members")
		})
	}
	g.Wait()
}

// BrowsePeer reads a page of a member's share.
func (n *Node) BrowsePeer(ctx context.Context, id, dir, cursor string) (peerapi.FilesPage, error) {
	p, ok := n.peerByID(id)
	if !ok {
		return peerapi.FilesPage{}, errors.New("unknown, revoked or unreachable member")
	}
	page, err := n.peerClient().Files(ctx, p.IP, dir, cursor)
	if err != nil {
		var se *peerapi.StatusError
		if errors.As(err, &se) {
			return page, fmt.Errorf("%s refused: %s", p.Name, se.Message)
		}
		return page, fmt.Errorf("%s does not answer: %w", p.Name, err)
	}
	return page, nil
}

// ScanNow triggers a share scan.
func (n *Node) ScanNow() { n.ix.TriggerScan() }
