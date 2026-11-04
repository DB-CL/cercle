// Package node assemble un nœud Cercle : base, clés, cercle, tunnel
// WireGuard, API pair-à-pair, index du partage, brides d'upload et file de
// téléchargement. Il fait tourner le gossip et réconcilie WireGuard avec la
// table des membres.
package node

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/DB-CL/cercle/internal/buildinfo"
	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/membership"
	"github.com/DB-CL/cercle/internal/peerapi"
	"github.com/DB-CL/cercle/internal/store"
	"github.com/DB-CL/cercle/internal/transfer"
	"github.com/DB-CL/cercle/internal/wg"
)

// Options configure un nœud. Les intervalles par défaut sont ceux du PRD ;
// les tests les raccourcissent.
type Options struct {
	Config          config.Config
	Logger          *slog.Logger
	GossipInterval  time.Duration // 30 s
	ResolveInterval time.Duration // 5 min, nouvelle résolution des noms DynDNS
	GossipFanout    int           // 2
}

// Node est un nœud Cercle.
type Node struct {
	cfg    config.Config
	opt    Options
	log    *slog.Logger
	st     *store.Store
	wgKey  wg.Key
	ms     *membership.Service
	ix     *index.Index
	up     *transfer.Uploads
	queue  *transfer.Manager
	client *peerapi.Client

	kick chan struct{}

	devMu   sync.Mutex // démarrage et arrêt du tunnel
	dev     *wg.Device
	devStop context.CancelFunc
	devDone chan struct{}

	recMu   sync.Mutex // une réconciliation à la fois
	rr      map[string]int
	revoked bool

	dnsMu sync.Mutex
	dns   map[string]dnsEntry
}

type dnsEntry struct {
	addrs []netip.AddrPort
	at    time.Time
}

// New ouvre la base et prépare le nœud ; Run le fait tourner.
func New(ctx context.Context, opt Options) (*Node, error) {
	if opt.GossipInterval == 0 {
		opt.GossipInterval = 30 * time.Second
	}
	if opt.ResolveInterval == 0 {
		opt.ResolveInterval = 5 * time.Minute
	}
	if opt.GossipFanout == 0 {
		opt.GossipFanout = 2
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	cfg := opt.Config
	n := &Node{opt: opt, log: opt.Logger, kick: make(chan struct{}, 1), rr: map[string]int{}, dns: map[string]dnsEntry{}}

	var err error
	if n.wgKey, err = wg.LoadOrCreateKey(filepath.Join(cfg.DataDir, "node.key")); err != nil {
		return nil, err
	}
	signKey, err := membership.LoadOrCreateSignKey(filepath.Join(cfg.DataDir, "node.sign"))
	if err != nil {
		return nil, err
	}
	if n.st, err = store.Open(ctx, filepath.Join(cfg.DataDir, "cercle.db")); err != nil {
		return nil, err
	}
	if err := cfg.ApplySettings(func(k string) (string, bool, error) { return n.st.Setting(ctx, k) }); err != nil {
		n.st.Close()
		return nil, err
	}
	n.cfg = cfg
	if n.ms, err = membership.New(ctx, n.st, membership.Identity{NodeID: n.wgKey.Public().String(), Sign: signKey}, n.log); err != nil {
		n.st.Close()
		return nil, err
	}
	if err := os.MkdirAll(cfg.DownloadsDir, 0o755); err != nil {
		n.st.Close()
		return nil, fmt.Errorf("dossier des téléchargements : %w", err)
	}
	n.ix = index.New(n.st, cfg.ShareDir, cfg.ShareExclude, n.log)
	n.up = transfer.NewUploads(cfg.UploadLimit, cfg.MaxUploads, cfg.UploadWindow)
	n.client = peerapi.NewClient(n.dial, "")
	n.queue = transfer.NewManager(transfer.Options{
		Store: n.st, Remote: n.remote(), Peers: n.peerByID, Dir: cfg.DownloadsDir,
		Concurrency: cfg.DownloadConcurrency, Limit: cfg.DownloadLimit, Window: cfg.DownloadWindow, Logger: n.log,
	})
	return n, nil
}

// remote renvoie un client pair-à-pair portant l'identifiant de cercle courant.
func (n *Node) remote() transfer.Remote { return remoteFunc{n} }

type remoteFunc struct{ n *Node }

func (r remoteFunc) Tree(ctx context.Context, peer netip.Addr, dir string, fn func(index.Entry) error) error {
	return r.n.peerClient().Tree(ctx, peer, dir, fn)
}

func (r remoteFunc) Download(ctx context.Context, peer netip.Addr, path string, offset int64, etag string) (*http.Response, error) {
	return r.n.peerClient().Download(ctx, peer, path, offset, etag)
}

func (n *Node) peerClient() *peerapi.Client {
	circle, _ := n.ms.Circle()
	return n.client.WithCircle(circle)
}

// Accesseurs pour l'interface.
func (n *Node) Store() *store.Store             { return n.st }
func (n *Node) Index() *index.Index             { return n.ix }
func (n *Node) Uploads() *transfer.Uploads      { return n.up }
func (n *Node) Queue() *transfer.Manager        { return n.queue }
func (n *Node) Membership() *membership.Service { return n.ms }
func (n *Node) Config() config.Config           { return n.cfg }
func (n *Node) PublicKey() wg.Key               { return n.wgKey.Public() }

// Close ferme la base (après Run).
func (n *Node) Close() error { return n.st.Close() }

func (n *Node) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	n.devMu.Lock()
	dev := n.dev
	n.devMu.Unlock()
	if dev == nil {
		return nil, errors.New("tunnel arrêté : ce nœud n'appartient à aucun cercle")
	}
	return dev.DialContext(ctx, network, addr)
}

func (n *Node) poke() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// Run fait tourner le nœud jusqu'à l'annulation de ctx.
func (n *Node) Run(ctx context.Context) error {
	var bg sync.WaitGroup
	defer bg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bg.Go(func() { n.ix.Run(ctx, n.cfg.ScanInterval) })
	bg.Go(func() {
		if err := n.queue.Run(ctx); err != nil {
			n.log.Error("file de téléchargement arrêtée", "err", err)
		}
	})
	defer n.stopDevice()

	if self, ok := n.ms.Self(); ok {
		if err := n.startDevice(self.TunnelIP); err != nil {
			return err
		}
	} else {
		n.log.Info("aucun cercle : créez-en un ou rejoignez-en un depuis l'interface")
	}

	t := time.NewTicker(n.opt.GossipInterval)
	defer t.Stop()
	last := time.Time{}
	for {
		n.reconcile(ctx)
		n.gossipRound(ctx)
		last = time.Now()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-n.kick:
			// Pas plus d'un tour par demi-seconde, même si les changements s'enchaînent.
			if d := 500*time.Millisecond - time.Since(last); d > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(d):
				}
			}
		}
	}
}

// startDevice démarre le tunnel sur addr et l'API pair-à-pair dessus.
func (n *Node) startDevice(addr netip.Addr) error {
	n.devMu.Lock()
	defer n.devMu.Unlock()
	if n.dev != nil {
		return nil
	}
	dev, err := wg.New(wg.Options{PrivateKey: n.wgKey, ListenPort: n.cfg.WGListenPort, Address: addr, Logger: n.log})
	if err != nil {
		return err
	}
	l, err := dev.Listen(peerapi.Port)
	if err != nil {
		dev.Close()
		return err
	}
	circle, _ := n.ms.Circle()
	srv := &peerapi.Server{
		CircleID:  circle,
		Directory: directory{n.ms},
		Info:      n.info,
		Index:     n.ix,
		Uploads:   n.up,
		Logger:    n.log.With("module", "peerapi"),
		InviteIP:  n.ms.InviteByTempIP,
		Join:      n.acceptJoin,
		Gossip:    n.handleGossip,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx, l); err != nil {
			n.log.Warn("API pair-à-pair arrêtée", "err", err)
		}
	}()
	n.dev, n.devStop, n.devDone = dev, cancel, done
	n.log.Info("tunnel démarré", "tunnel_ip", addr, "listen_port", n.cfg.WGListenPort)
	return nil
}

func (n *Node) stopDevice() {
	n.devMu.Lock()
	defer n.devMu.Unlock()
	if n.dev == nil {
		return
	}
	n.devStop()
	n.dev.Close() // coupe les connexions en cours, l'arrêt du serveur est immédiat
	<-n.devDone
	n.dev = nil
}

func (n *Node) device() *wg.Device {
	n.devMu.Lock()
	defer n.devMu.Unlock()
	return n.dev
}

func (n *Node) info(ctx context.Context) peerapi.Info {
	size, files, last := n.ix.Totals(ctx)
	name := n.cfg.NodeName
	if self, ok := n.ms.Self(); ok {
		name = self.Name
	}
	return peerapi.Info{Name: name, Version: buildinfo.Version, ShareSize: size, ShareFiles: files, LastScan: last}
}

// directory adapte membership à peerapi.Directory.
type directory struct{ ms *membership.Service }

func (d directory) MemberByIP(ip netip.Addr) (peerapi.Member, bool) {
	m, ok := d.ms.ByIP(ip)
	return peerapi.Member{NodeID: m.NodeID, Name: m.DisplayName, TunnelIP: m.TunnelIP}, ok
}

func (n *Node) peerByID(id string) (transfer.Peer, bool) {
	m, ok := n.ms.Member(id)
	if !ok || m.Revoked || m.Conflict || m.Self {
		return transfer.Peer{}, false
	}
	return transfer.Peer{ID: m.NodeID, Name: m.DisplayName, IP: m.TunnelIP}, true
}

func (n *Node) handleGossip(ctx context.Context, from peerapi.Member, t membership.Table) (membership.Table, error) {
	changed, err := n.ms.Merge(ctx, t)
	if err != nil {
		return membership.Table{}, err
	}
	if changed {
		n.poke()
	}
	return n.ms.Table(), nil
}

func (n *Node) acceptJoin(ctx context.Context, tempIP netip.Addr, req membership.JoinRequest) (membership.JoinResponse, error) {
	resp, err := n.ms.AcceptJoin(ctx, tempIP, req)
	if err == nil {
		n.poke() // la vraie clé du nouveau membre au prochain tour, dans la demi-seconde
	}
	return resp, err
}

// CreateCircle fonde un cercle.
func (n *Node) CreateCircle(ctx context.Context) error {
	if err := n.ms.Create(ctx, n.cfg.TunnelSubnet, n.cfg.NodeName, n.cfg.WGPublicEndpoints); err != nil {
		return err
	}
	self, _ := n.ms.Self()
	if err := n.startDevice(self.TunnelIP); err != nil {
		return err
	}
	n.log.Info("cercle créé", "tunnel_ip", self.TunnelIP)
	return nil
}

// Invite crée un code d'invitation.
func (n *Node) Invite(ctx context.Context) (string, membership.Invite, error) {
	temp, err := wg.GenerateKey()
	if err != nil {
		return "", membership.Invite{}, err
	}
	code, inv, err := n.ms.NewInvite(ctx, n.cfg.InviteTTL, n.selfEndpoints(), temp.String(), temp.Public().String())
	if err != nil {
		return "", membership.Invite{}, err
	}
	n.reconcile(ctx) // la clé temporaire doit être déclarée avant que l'invité n'arrive
	return code.Encode(), inv, nil
}

// selfEndpoints : endpoints annoncés, puis ceux sous lesquels les autres nous voient.
func (n *Node) selfEndpoints() []string {
	eps := slices.Clone(n.cfg.WGPublicEndpoints)
	me := n.wgKey.Public().String()
	for _, m := range n.ms.Members() {
		if ep, ok := m.Observed[me]; ok && !m.Revoked && !slices.Contains(eps, ep) {
			eps = append(eps, ep)
		}
	}
	return eps
}

// Revoke révoque un membre ; les autres nœuds l'apprennent par gossip.
func (n *Node) Revoke(ctx context.Context, id string) error {
	if err := n.ms.Revoke(ctx, id); err != nil {
		return err
	}
	n.reconcile(ctx)
	n.poke()
	return nil
}

// Join rejoint un cercle avec un code d'invitation.
func (n *Node) Join(ctx context.Context, raw string) error {
	code, err := membership.ParseCode(raw)
	if err != nil {
		return err
	}
	req, err := n.ms.PrepareJoin(code, n.cfg.NodeName, n.cfg.WGPublicEndpoints)
	if err != nil {
		return err
	}
	resp, err := n.joinWithTempKey(ctx, code, req)
	if err != nil {
		return err
	}
	if err := n.ms.CompleteJoin(ctx, code, req, resp); err != nil {
		return err
	}
	if err := n.startDevice(code.Assigned); err != nil {
		return err
	}
	n.log.Info("cercle rejoint", "tunnel_ip", code.Assigned, "invited_by", code.InviterNom)
	n.reconcile(ctx)
	n.poke()
	return nil
}

// joinWithTempKey monte un tunnel provisoire avec la clé temporaire du code
// et envoie la demande d'adhésion, en essayant chaque endpoint de l'invitant.
func (n *Node) joinWithTempKey(ctx context.Context, code membership.Code, req membership.JoinRequest) (membership.JoinResponse, error) {
	tempKey, err := wg.ParseKey(code.TempKey)
	if err != nil {
		return membership.JoinResponse{}, membership.ErrBadCode
	}
	inviter, err := wg.ParseKey(code.Inviter)
	if err != nil {
		return membership.JoinResponse{}, membership.ErrBadCode
	}
	if n.device() != nil {
		return membership.JoinResponse{}, membership.ErrAlreadyInCircle
	}
	dev, err := wg.New(wg.Options{PrivateKey: tempKey, ListenPort: n.cfg.WGListenPort, Address: code.TempIP, Logger: n.log})
	if err != nil {
		return membership.JoinResponse{}, err
	}
	defer dev.Close()
	client := peerapi.NewClient(dev.DialContext, code.Circle)
	var lastErr error = errors.New("aucun endpoint dans le code")
	for _, hp := range code.Endpoints {
		for _, ep := range n.resolve(ctx, hp) {
			if err := dev.SetPeers([]wg.Peer{{PublicKey: inviter, TunnelIP: code.InviterIP, Endpoint: ep}}); err != nil {
				return membership.JoinResponse{}, err
			}
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			resp, err := client.Join(rctx, code.InviterIP, req)
			cancel()
			switch {
			case err == nil:
				return resp, nil
			case errors.Is(err, membership.ErrInviteUsed), errors.Is(err, membership.ErrInviteExpired), errors.Is(err, membership.ErrInviteUnknown):
				return membership.JoinResponse{}, err
			}
			n.log.Info("invitant injoignable par cet endpoint", "endpoint", ep, "err", err)
			lastErr = err
			// Une autre adresse : la session WireGuard repart de zéro.
			if err := dev.SetPeers(nil); err != nil {
				return membership.JoinResponse{}, err
			}
		}
	}
	return membership.JoinResponse{}, fmt.Errorf("impossible de joindre %s : %w", code.InviterNom, lastErr)
}

// resolve transforme « hôte:port » en adresses, avec un cache de ResolveInterval.
func (n *Node) resolve(ctx context.Context, hostport string) []netip.AddrPort {
	if ap, err := netip.ParseAddrPort(hostport); err == nil {
		return []netip.AddrPort{netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())}
	}
	n.dnsMu.Lock()
	e, ok := n.dns[hostport]
	n.dnsMu.Unlock()
	if ok && time.Since(e.at) < n.opt.ResolveInterval {
		return e.addrs
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		n.log.Debug("résolution impossible", "host", host, "err", err)
		return e.addrs // on garde l'ancienne réponse
	}
	var out []netip.AddrPort
	for _, ip := range ips {
		if ap, err := netip.ParseAddrPort(net.JoinHostPort(ip.Unmap().String(), port)); err == nil {
			out = append(out, ap)
		}
	}
	n.dnsMu.Lock()
	n.dns[hostport] = dnsEntry{addrs: out, at: time.Now()}
	n.dnsMu.Unlock()
	return out
}

// reconcile aligne WireGuard sur la table des membres, publie les endpoints
// observés et règle un éventuel conflit d'adresse.
func (n *Node) reconcile(ctx context.Context) {
	n.recMu.Lock()
	defer n.recMu.Unlock()
	dev := n.device()
	if dev == nil || ctx.Err() != nil {
		return
	}
	if ip, err := n.ms.ResolveConflict(ctx); err != nil {
		n.log.Error("conflit d'adresse", "err", err)
	} else if ip.IsValid() {
		n.stopDevice()
		if err := n.startDevice(ip); err != nil {
			n.log.Error("redémarrage du tunnel", "err", err)
			return
		}
		dev = n.device()
	}
	self, ok := n.ms.Self()
	if !ok {
		return
	}
	if self.Revoked {
		if !n.revoked {
			n.log.Warn("ce nœud a été révoqué du cercle", "by", self.RevokedBy)
			n.revoked = true
		}
		_ = dev.SetPeers(nil)
		return
	}

	status := map[wg.Key]wg.PeerStatus{}
	if st, err := dev.Peers(); err == nil {
		for _, p := range st {
			status[p.PublicKey] = p
		}
	}
	now := time.Now()
	members := n.ms.Members()
	var peers []wg.Peer
	observed := map[string]string{}
	for _, m := range members {
		if m.Self || m.Revoked || m.Conflict {
			continue
		}
		key, err := wg.ParseKey(m.NodeID)
		if err != nil {
			continue
		}
		st := status[key]
		if st.Online(now) && st.Endpoint.IsValid() {
			observed[m.NodeID] = st.Endpoint.String()
		}
		peers = append(peers, wg.Peer{PublicKey: key, TunnelIP: m.TunnelIP, Endpoint: n.nextEndpoint(ctx, m, members, st.Online(now))})
	}
	for _, inv := range n.ms.TempPeers(ctx) {
		if key, err := wg.ParseKey(inv.TempPubKey); err == nil {
			peers = append(peers, wg.Peer{PublicKey: key, TunnelIP: inv.TempIP})
		}
	}
	if err := dev.SetPeers(peers); err != nil {
		n.log.Error("réconciliation WireGuard", "err", err)
	}
	changed, err := n.ms.UpdateSelf(ctx, func(b *membership.Body) {
		b.Announced = slices.Clone(n.cfg.WGPublicEndpoints)
		if len(observed) == 0 {
			b.Observed = nil
		} else {
			b.Observed = observed
		}
	})
	if err != nil {
		n.log.Error("mise à jour de la fiche", "err", err)
	} else if changed {
		n.poke()
	}
}

// nextEndpoint choisit l'endpoint à essayer pour un pair : ses endpoints
// annoncés, puis ceux sous lesquels les autres membres le voient. Tant que le
// pair reste injoignable, on passe au suivant à chaque tour.
func (n *Node) nextEndpoint(ctx context.Context, m membership.Member, members []membership.Member, online bool) netip.AddrPort {
	var cands []netip.AddrPort
	add := func(ap netip.AddrPort) {
		if ap.IsValid() && !slices.Contains(cands, ap) {
			cands = append(cands, ap)
		}
	}
	for _, hp := range m.Announced {
		for _, ap := range n.resolve(ctx, hp) {
			add(ap)
		}
	}
	for _, o := range members {
		if ep, ok := o.Observed[m.NodeID]; ok && !o.Revoked {
			if ap, err := netip.ParseAddrPort(ep); err == nil {
				add(ap)
			}
		}
	}
	if len(cands) == 0 {
		return netip.AddrPort{}
	}
	if online {
		return cands[0] // ignoré par SetPeers : la session vivante garde son endpoint
	}
	i := n.rr[m.NodeID] % len(cands)
	n.rr[m.NodeID]++
	return cands[i]
}

// gossipRound échange la table avec quelques pairs tirés au hasard, en
// préférant ceux qui sont en ligne.
func (n *Node) gossipRound(ctx context.Context) {
	dev := n.device()
	self, ok := n.ms.Self()
	if dev == nil || !ok || self.Revoked || ctx.Err() != nil {
		return
	}
	online := map[string]bool{}
	if st, err := dev.Peers(); err == nil {
		for _, p := range st {
			online[p.PublicKey.String()] = p.Online(time.Now())
		}
	}
	var on, off []membership.Member
	for _, m := range n.ms.Members() {
		if m.Self || m.Revoked || m.Conflict {
			continue
		}
		if online[m.NodeID] {
			on = append(on, m)
		} else {
			off = append(off, m)
		}
	}
	shuffle(on)
	shuffle(off)
	targets := append(on, off...)
	if len(targets) > n.opt.GossipFanout {
		targets = targets[:n.opt.GossipFanout]
	}
	circle, _ := n.ms.Circle()
	client := n.client.WithCircle(circle)
	var g sync.WaitGroup
	changed := false
	var mu sync.Mutex
	for _, m := range targets {
		g.Go(func() {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			t, err := client.Gossip(rctx, m.TunnelIP, n.ms.Table())
			if err != nil {
				n.log.Debug("gossip impossible", "peer", m.DisplayName, "err", err)
				return
			}
			c, err := n.ms.Merge(ctx, t)
			if err != nil {
				n.log.Error("fusion de table", "peer", m.DisplayName, "err", err)
				return
			}
			mu.Lock()
			changed = changed || c
			mu.Unlock()
		})
	}
	g.Wait()
	if changed {
		n.reconcile(ctx)
	}
}

func shuffle(ms []membership.Member) {
	for i := len(ms) - 1; i > 0; i-- {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		ms[i], ms[j.Int64()] = ms[j.Int64()], ms[i]
	}
}

// PeerState est l'état réseau d'un membre, pour l'interface.
type PeerState struct {
	membership.Member
	Online        bool      `json:"online"`
	LastHandshake time.Time `json:"last_handshake,omitzero"`
	Endpoint      string    `json:"endpoint,omitempty"`
	RxBytes       uint64    `json:"rx_bytes"`
	TxBytes       uint64    `json:"tx_bytes"`
}

// Peers renvoie les membres avec leur état WireGuard.
func (n *Node) Peers() []PeerState {
	status := map[string]wg.PeerStatus{}
	if dev := n.device(); dev != nil {
		if st, err := dev.Peers(); err == nil {
			for _, p := range st {
				status[p.PublicKey.String()] = p
			}
		}
	}
	var out []PeerState
	for _, m := range n.ms.Members() {
		ps := PeerState{Member: m}
		if st, ok := status[m.NodeID]; ok {
			ps.Online, ps.LastHandshake, ps.RxBytes, ps.TxBytes = st.Online(time.Now()), st.LastHandshake, st.RxBytes, st.TxBytes
			if st.Endpoint.IsValid() {
				ps.Endpoint = st.Endpoint.String()
			}
		}
		out = append(out, ps)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Self && !out[j].Self })
	return out
}

// TunnelUp dit si le tunnel tourne.
func (n *Node) TunnelUp() bool { return n.device() != nil }
