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

// DefaultMTU leaves room for WireGuard headers on an Ethernet or IPv6 link.
const DefaultMTU = 1420

// KeepaliveInterval keeps NAT mappings open (seconds).
const KeepaliveInterval = 25

// handshakeFresh: below this age the session is alive and the endpoint learned
// by roaming must not be overwritten. WireGuard renews the handshake every
// 2 minutes on an active link; with a 25 s keepalive, a healthy session is
// never older than 2 min 45 s.
const handshakeFresh = 3 * time.Minute

// Options configures a Device.
type Options struct {
	PrivateKey Key
	ListenPort int        // 0 = UDP port chosen by the system
	Address    netip.Addr // node tunnel address
	MTU        int        // 0 = DefaultMTU
	Logger     *slog.Logger

	// upstreamNetstack forces wireguard-go's original netstack (bench comparison).
	upstreamNetstack bool
}

// Peer is the desired state of a peer.
type Peer struct {
	PublicKey Key
	TunnelIP  netip.Addr     // allowed_ip as /32
	Endpoint  netip.AddrPort // optional; invalid = unknown
}

// PeerStatus is the observed state of a peer.
type PeerStatus struct {
	PublicKey     Key
	Endpoint      netip.AddrPort // actual endpoint, learned by roaming if applicable
	LastHandshake time.Time      // zero = never
	RxBytes       uint64
	TxBytes       uint64
	AllowedIPs    []netip.Prefix
}

// Online reports whether the session with this peer is alive.
func (s PeerStatus) Online(now time.Time) bool {
	return !s.LastHandshake.IsZero() && now.Sub(s.LastHandshake) < handshakeFresh
}

// Device is an embedded WireGuard interface with its TCP/IP stack.
type Device struct {
	dev    *device.Device
	listen func(port uint16) (net.Listener, error)
	dial   func(ctx context.Context, to netip.AddrPort) (net.Conn, error)
	addr   netip.Addr
	log    *slog.Logger
	mu     sync.Mutex // serializes reconciliations
}

// New starts WireGuard. No peer is configured: see SetPeers.
func New(opts Options) (*Device, error) {
	if !opts.Address.IsValid() {
		return nil, errors.New("missing tunnel address")
	}
	if opts.PrivateKey.IsZero() {
		return nil, errors.New("missing private key")
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
			return nil, fmt.Errorf("creating the netstack: %w", err)
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
			return nil, fmt.Errorf("creating the netstack: %w", err)
		}
		tdev, d.listen, d.dial = nt, nt.listenTCP, nt.dialTCP
	}
	logger := &device.Logger{
		Verbosef: func(format string, args ...any) { log.Debug(fmt.Sprintf(format, args...)) },
		Errorf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			// Expected for a pending invitation key or a peer not located yet.
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
		return nil, fmt.Errorf("configuring WireGuard: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("starting WireGuard (UDP port %d): %w", opts.ListenPort, err)
	}
	d.dev = dev
	return d, nil
}

// Address returns the node's tunnel address.
func (d *Device) Address() netip.Addr { return d.addr }

// Listen opens a TCP port on the tunnel address, inside the netstack only:
// nothing is bound to a host interface.
func (d *Device) Listen(port int) (net.Listener, error) {
	l, err := d.listen(uint16(port))
	if err != nil {
		return nil, fmt.Errorf("listening on %s:%d in the tunnel: %w", d.addr, port, err)
	}
	return l, nil
}

// DialContext opens a TCP connection through the tunnel. The address is a
// literal IP and port: the tunnel has no DNS.
func (d *Device) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("network %q not supported in the tunnel", network)
	}
	to, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid tunnel address %q: %w", address, err)
	}
	return d.dial(ctx, to)
}

// Close stops WireGuard and releases the UDP port.
func (d *Device) Close() { d.dev.Close() }

// ListenPort returns the UDP port actually in use.
func (d *Device) ListenPort() (int, error) {
	raw, err := d.dev.IpcGet()
	if err != nil {
		return 0, fmt.Errorf("reading WireGuard state: %w", err)
	}
	for _, line := range strings.Split(raw, "\n") {
		if v, ok := strings.CutPrefix(line, "listen_port="); ok {
			return strconv.Atoi(v)
		}
	}
	return 0, errors.New("unknown listen port")
}

// SetPrivateKey changes the node key live (switch after an invitation).
func (d *Device) SetPrivateKey(k Key) error {
	if err := d.dev.IpcSet("private_key=" + k.Hex() + "\n"); err != nil {
		return fmt.Errorf("changing the key: %w", err)
	}
	return nil
}

// Peers returns the observed state of every peer, sorted by key.
func (d *Device) Peers() ([]PeerStatus, error) {
	raw, err := d.dev.IpcGet()
	if err != nil {
		return nil, fmt.Errorf("reading WireGuard state: %w", err)
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
			continue // device-level lines
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

// SetPeers reconciles WireGuard with the desired list: adds, updates and
// removes peers without restarting.
//
// The desired endpoint is only applied to a new peer or a dead session. A live
// session keeps the endpoint learned by roaming: it is often the only one that
// works for a peer behind NAT, and overwriting it every 30 s would cut the link.
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
			return fmt.Errorf("invalid peer: key %q, address %v", p.PublicKey, p.TunnelIP)
		}
		if want[p.PublicKey] {
			return fmt.Errorf("duplicate peer: %s", p.PublicKey)
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
		return fmt.Errorf("reconciling peers: %w", err)
	}
	return nil
}
