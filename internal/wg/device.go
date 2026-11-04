package wg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// DefaultMTU laisse la place aux en-têtes WireGuard sur un lien Ethernet ou IPv6.
const DefaultMTU = 1420

// KeepaliveInterval maintient les associations NAT ouvertes (secondes).
const KeepaliveInterval = 25

// handshakeFresh : en deçà, la session est vivante et l'endpoint appris par
// roaming ne doit pas être écrasé. WireGuard renouvelle la poignée de main
// toutes les 2 minutes sur un lien actif ; keepalive à 25 s compris, une
// session saine n'a jamais plus de 2 min 45.
const handshakeFresh = 3 * time.Minute

// Options configure un Device.
type Options struct {
	PrivateKey Key
	ListenPort int        // 0 = port UDP choisi par le système
	Address    netip.Addr // adresse de tunnel du nœud
	MTU        int        // 0 = DefaultMTU
	Logger     *slog.Logger

	// upstreamNetstack force le netstack d'origine de wireguard-go (comparaison au banc).
	upstreamNetstack bool
}

// Peer est l'état voulu d'un pair.
type Peer struct {
	PublicKey Key
	TunnelIP  netip.Addr     // allowed_ip en /32
	Endpoint  netip.AddrPort // facultatif ; invalide = inconnu
}

// PeerStatus est l'état observé d'un pair.
type PeerStatus struct {
	PublicKey     Key
	Endpoint      netip.AddrPort // endpoint réel, appris par roaming le cas échéant
	LastHandshake time.Time      // zéro = jamais
	RxBytes       uint64
	TxBytes       uint64
	AllowedIPs    []netip.Prefix
}

// Online dit si la session avec ce pair est vivante.
func (s PeerStatus) Online(now time.Time) bool {
	return !s.LastHandshake.IsZero() && now.Sub(s.LastHandshake) < handshakeFresh
}

// Device est une interface WireGuard embarquée avec sa pile TCP/IP.
type Device struct {
	dev    *device.Device
	listen func(port uint16) (net.Listener, error)
	dial   func(ctx context.Context, to netip.AddrPort) (net.Conn, error)
	addr   netip.Addr
	log    *slog.Logger
	mu     sync.Mutex // sérialise les réconciliations
}

// New démarre WireGuard. Aucun pair n'est configuré : voir SetPeers.
func New(opts Options) (*Device, error) {
	if !opts.Address.IsValid() {
		return nil, errors.New("adresse de tunnel manquante")
	}
	if opts.PrivateKey.IsZero() {
		return nil, errors.New("clé privée manquante")
	}
	if opts.MTU == 0 {
		opts.MTU = DefaultMTU
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("module", "wg")

	d := &Device{addr: opts.Address, log: log}
	var tdev tun.Device
	if opts.upstreamNetstack {
		td, tnet, err := netstack.CreateNetTUN([]netip.Addr{opts.Address}, nil, opts.MTU)
		if err != nil {
			return nil, fmt.Errorf("création du netstack : %w", err)
		}
		tdev = td
		d.listen = func(port uint16) (net.Listener, error) {
			return tnet.ListenTCPAddrPort(netip.AddrPortFrom(opts.Address, port))
		}
		d.dial = func(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
			return tnet.DialContextTCPAddrPort(ctx, to)
		}
	} else {
		nt, err := newNetTUN(opts.Address, opts.MTU)
		if err != nil {
			return nil, fmt.Errorf("création du netstack : %w", err)
		}
		tdev, d.listen, d.dial = nt, nt.listenTCP, nt.dialTCP
	}
	logger := &device.Logger{
		Verbosef: func(format string, args ...any) { log.Debug(fmt.Sprintf(format, args...)) },
		Errorf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			// Normal pour une clé d'invitation en attente ou un pair pas encore localisé.
			if strings.Contains(msg, "no known endpoint") {
				log.Debug(msg)
				return
			}
			log.Warn(msg)
		},
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), logger)
	conf := fmt.Sprintf("private_key=%s\nlisten_port=%d\n", opts.PrivateKey.Hex(), opts.ListenPort)
	if err := dev.IpcSet(conf); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configuration de WireGuard : %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("démarrage de WireGuard (port UDP %d) : %w", opts.ListenPort, err)
	}
	d.dev = dev
	return d, nil
}

// Address renvoie l'adresse de tunnel du nœud.
func (d *Device) Address() netip.Addr { return d.addr }

// Listen ouvre un port TCP sur l'adresse de tunnel, dans le netstack uniquement :
// rien n'est lié à une interface de l'hôte.
func (d *Device) Listen(port int) (net.Listener, error) {
	l, err := d.listen(uint16(port))
	if err != nil {
		return nil, fmt.Errorf("écoute sur %s:%d dans le tunnel : %w", d.addr, port, err)
	}
	return l, nil
}

// DialContext ouvre une connexion TCP à travers le tunnel. L'adresse est une
// IP littérale suivie du port : le tunnel n'a pas de DNS.
func (d *Device) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("réseau %q non géré dans le tunnel", network)
	}
	to, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("adresse de tunnel invalide %q : %w", address, err)
	}
	return d.dial(ctx, to)
}

// Close arrête WireGuard et libère le port UDP.
func (d *Device) Close() { d.dev.Close() }

// ListenPort renvoie le port UDP réellement utilisé.
func (d *Device) ListenPort() (int, error) {
	raw, err := d.dev.IpcGet()
	if err != nil {
		return 0, fmt.Errorf("lecture de l'état WireGuard : %w", err)
	}
	for _, line := range strings.Split(raw, "\n") {
		if v, ok := strings.CutPrefix(line, "listen_port="); ok {
			return strconv.Atoi(v)
		}
	}
	return 0, errors.New("port d'écoute inconnu")
}

// SetPrivateKey change la clé du nœud à chaud (bascule après une invitation).
func (d *Device) SetPrivateKey(k Key) error {
	if err := d.dev.IpcSet("private_key=" + k.Hex() + "\n"); err != nil {
		return fmt.Errorf("changement de clé : %w", err)
	}
	return nil
}

// Peers renvoie l'état observé de tous les pairs, triés par clé.
func (d *Device) Peers() ([]PeerStatus, error) {
	raw, err := d.dev.IpcGet()
	if err != nil {
		return nil, fmt.Errorf("lecture de l'état WireGuard : %w", err)
	}
	return parseIpcGet(raw)
}

func parseIpcGet(raw string) ([]PeerStatus, error) {
	var (
		peers    []PeerStatus
		cur      *PeerStatus
		sec, nse int64
	)
	flush := func() {
		if cur != nil {
			if sec != 0 || nse != 0 {
				cur.LastHandshake = time.Unix(sec, nse)
			}
			peers = append(peers, *cur)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		if key == "public_key" {
			flush()
			k, err := parseHexKey(val)
			if err != nil {
				return nil, err
			}
			cur, sec, nse = &PeerStatus{PublicKey: k}, 0, 0
			continue
		}
		if cur == nil {
			continue // lignes propres au device
		}
		switch key {
		case "endpoint":
			if ap, err := netip.ParseAddrPort(val); err == nil {
				cur.Endpoint = ap
			}
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(val, 10, 64)
		case "last_handshake_time_nsec":
			nse, _ = strconv.ParseInt(val, 10, 64)
		case "rx_bytes":
			cur.RxBytes, _ = strconv.ParseUint(val, 10, 64)
		case "tx_bytes":
			cur.TxBytes, _ = strconv.ParseUint(val, 10, 64)
		case "allowed_ip":
			if p, err := netip.ParsePrefix(val); err == nil {
				cur.AllowedIPs = append(cur.AllowedIPs, p)
			}
		}
	}
	flush()
	sort.Slice(peers, func(i, j int) bool { return peers[i].PublicKey.Hex() < peers[j].PublicKey.Hex() })
	return peers, sc.Err()
}

// SetPeers réconcilie WireGuard avec la liste voulue : ajoute, met à jour,
// retire, sans redémarrage.
//
// L'endpoint voulu n'est appliqué que si le pair est nouveau ou si sa session
// est morte. Une session vivante garde l'endpoint appris par roaming : c'est
// souvent le seul qui marche pour un pair derrière NAT, et l'écraser toutes les
// 30 s couperait le lien.
func (d *Device) SetPeers(peers []Peer) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	current, err := d.Peers()
	if err != nil {
		return err
	}
	now := time.Now()
	byKey := make(map[Key]PeerStatus, len(current))
	for _, s := range current {
		byKey[s.PublicKey] = s
	}

	var b strings.Builder
	want := make(map[Key]bool, len(peers))
	for _, p := range peers {
		if p.PublicKey.IsZero() || !p.TunnelIP.Is4() {
			return fmt.Errorf("pair invalide : clé %q, adresse %v", p.PublicKey, p.TunnelIP)
		}
		if want[p.PublicKey] {
			return fmt.Errorf("pair en double : %s", p.PublicKey)
		}
		want[p.PublicKey] = true
		fmt.Fprintf(&b, "public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/32\npersistent_keepalive_interval=%d\n",
			p.PublicKey.Hex(), p.TunnelIP, KeepaliveInterval)
		st, known := byKey[p.PublicKey]
		if p.Endpoint.IsValid() && p.Endpoint != st.Endpoint && (!known || !st.Online(now)) {
			fmt.Fprintf(&b, "endpoint=%s\n", p.Endpoint)
		}
	}
	for k := range byKey {
		if !want[k] {
			fmt.Fprintf(&b, "public_key=%s\nremove=true\n", k.Hex())
		}
	}
	if b.Len() == 0 {
		return nil
	}
	if err := d.dev.IpcSet(b.String()); err != nil {
		return fmt.Errorf("réconciliation des pairs : %w", err)
	}
	return nil
}
