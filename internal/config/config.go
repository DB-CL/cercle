// Package config lit la configuration du nœud depuis les variables d'environnement.
//
// Les réglages modifiables à chaud (limites, créneaux) seront ensuite surchargés
// par les valeurs stockées en base (jalon J2) : la base prime après la première
// modification depuis l'interface.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config regroupe tous les réglages du nœud.
type Config struct {
	NodeName          string
	WGListenPort      int
	WGPublicEndpoints []string // hôte:port, résolus plus tard (les noms DynDNS changent)
	TunnelSubnet      netip.Prefix

	UIListen   string
	UIPassword string

	UploadLimit  int64 // octets par seconde, 0 = illimité
	MaxUploads   int
	UploadWindow *Window // nil = toujours ouvert

	DownloadLimit       int64
	DownloadConcurrency int
	DownloadWindow      *Window

	ScanInterval time.Duration
	ShareExclude []string
	InviteTTL    time.Duration
	LogLevel     slog.Level

	// Chemins des volumes. Fixes dans l'image, surchargeables pour le développement.
	DataDir      string
	ShareDir     string
	DownloadsDir string
}

// Load lit la configuration. getenv vaut en général os.Getenv ; les tests
// passent une fonction qui lit une table.
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	c := Config{
		NodeName:            p.str("NODE_NAME", defaultHostname()),
		WGListenPort:        p.port("WG_LISTEN_PORT", 51820),
		WGPublicEndpoints:   p.endpoints("WG_PUBLIC_ENDPOINTS"),
		TunnelSubnet:        p.prefix("TUNNEL_SUBNET", "10.42.0.0/24"),
		UIListen:            p.str("UI_LISTEN", ":8080"),
		UIPassword:          getenv("UI_PASSWORD"),
		UploadLimit:         p.size("UPLOAD_LIMIT"),
		MaxUploads:          p.positive("MAX_UPLOADS", 3),
		UploadWindow:        p.window("UPLOAD_WINDOW"),
		DownloadLimit:       p.size("DOWNLOAD_LIMIT"),
		DownloadConcurrency: p.positive("DOWNLOAD_CONCURRENCY", 2),
		DownloadWindow:      p.window("DOWNLOAD_WINDOW"),
		ScanInterval:        p.duration("SCAN_INTERVAL", time.Hour),
		ShareExclude:        p.list("SHARE_EXCLUDE"),
		InviteTTL:           p.duration("INVITE_TTL", 72*time.Hour),
		LogLevel:            p.level("LOG_LEVEL"),
		DataDir:             p.str("DATA_DIR", "/data"),
		ShareDir:            p.str("SHARE_DIR", "/share"),
		DownloadsDir:        p.str("DOWNLOADS_DIR", "/downloads"),
	}
	if err := errors.Join(p.errs...); err != nil {
		return Config{}, fmt.Errorf("configuration invalide : %w", err)
	}
	return c, nil
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "cercle"
	}
	return h
}

// parser accumule les erreurs pour toutes les signaler d'un coup.
type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(name, val string, err error) {
	p.errs = append(p.errs, fmt.Errorf("%s=%q : %w", name, val, err))
}

func (p *parser) str(name, def string) string {
	if v := strings.TrimSpace(p.getenv(name)); v != "" {
		return v
	}
	return def
}

func (p *parser) port(name string, def int) int {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		p.fail(name, v, errors.New("port attendu entre 1 et 65535"))
		return def
	}
	return n
}

func (p *parser) positive(name string, def int) int {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		p.fail(name, v, errors.New("entier positif attendu"))
		return def
	}
	return n
}

func (p *parser) duration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.fail(name, v, errors.New("durée positive attendue (ex. 1h, 30m)"))
		return def
	}
	return d
}

func (p *parser) size(name string) int64 {
	v := p.getenv(name)
	n, err := ParseRate(v)
	if err != nil {
		p.fail(name, v, err)
	}
	return n
}

func (p *parser) window(name string) *Window {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return nil
	}
	w, err := ParseWindow(v)
	if err != nil {
		p.fail(name, v, err)
		return nil
	}
	return &w
}

func (p *parser) prefix(name, def string) netip.Prefix {
	v := p.str(name, def)
	pfx, err := netip.ParsePrefix(v)
	if err != nil || !pfx.Addr().Is4() || pfx.Bits() > 29 || pfx.Bits() < 8 {
		p.fail(name, v, errors.New("sous-réseau IPv4 attendu, de /8 à /29 (ex. 10.42.0.0/24)"))
		return netip.MustParsePrefix(def)
	}
	return pfx.Masked()
}

func (p *parser) list(name string) []string {
	var out []string
	for _, s := range strings.Split(p.getenv(name), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (p *parser) endpoints(name string) []string {
	eps := p.list(name)
	for _, ep := range eps {
		host, port, err := net.SplitHostPort(ep)
		if err == nil && host != "" {
			if n, perr := strconv.Atoi(port); perr == nil && n > 0 && n <= 65535 {
				continue
			}
		}
		p.fail(name, ep, errors.New("hôte:port attendu (IPv6 entre crochets : [2001:db8::1]:51820)"))
	}
	return eps
}

func (p *parser) level(name string) slog.Level {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return slog.LevelInfo
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(name, v, errors.New("debug, info, warn ou error attendu"))
		return slog.LevelInfo
	}
	return l
}

// Clés des réglages modifiables depuis l'interface, stockés en base.
const (
	SettingNodeName            = "node_name"
	SettingUploadLimit         = "upload_limit"
	SettingMaxUploads          = "max_uploads"
	SettingUploadWindow        = "upload_window"
	SettingDownloadLimit       = "download_limit"
	SettingDownloadConcurrency = "download_concurrency"
	SettingDownloadWindow      = "download_window"
)

// ApplySettings remplace les valeurs de l'environnement par celles stockées en
// base : après la première modification depuis l'interface, la base prime.
// Une valeur vide pour un créneau signifie « toujours ouvert ».
func (c *Config) ApplySettings(get func(key string) (string, bool, error)) error {
	var errs []error
	apply := func(key string, set func(string) error) {
		v, ok, err := get(key)
		if err != nil {
			errs = append(errs, err)
			return
		}
		if !ok {
			return
		}
		if err := set(v); err != nil {
			errs = append(errs, fmt.Errorf("réglage %s=%q : %w", key, v, err))
		}
	}
	rateSetter := func(dst *int64) func(string) error {
		return func(v string) error {
			n, err := ParseRate(v)
			if err == nil {
				*dst = n
			}
			return err
		}
	}
	intSetter := func(dst *int) func(string) error {
		return func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return errors.New("entier positif attendu")
			}
			*dst = n
			return nil
		}
	}
	windowSetter := func(dst **Window) func(string) error {
		return func(v string) error {
			if strings.TrimSpace(v) == "" {
				*dst = nil
				return nil
			}
			w, err := ParseWindow(v)
			if err == nil {
				*dst = &w
			}
			return err
		}
	}
	apply(SettingNodeName, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("nom vide")
		}
		c.NodeName = v
		return nil
	})
	apply(SettingUploadLimit, rateSetter(&c.UploadLimit))
	apply(SettingMaxUploads, intSetter(&c.MaxUploads))
	apply(SettingUploadWindow, windowSetter(&c.UploadWindow))
	apply(SettingDownloadLimit, rateSetter(&c.DownloadLimit))
	apply(SettingDownloadConcurrency, intSetter(&c.DownloadConcurrency))
	apply(SettingDownloadWindow, windowSetter(&c.DownloadWindow))
	return errors.Join(errs...)
}
