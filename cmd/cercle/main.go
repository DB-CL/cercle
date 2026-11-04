// Commande cercle : partage privé de fichiers entre NAS sur WireGuard.
//
//	cercle serve    démarre le nœud (par défaut)
//	cercle pubkey   affiche la clé publique du nœud (la crée au besoin)
//	cercle version  affiche la version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // l'image scratch n'a pas de zoneinfo

	"github.com/DB-CL/cercle/internal/buildinfo"
	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/peerapi"
	"github.com/DB-CL/cercle/internal/store"
	"github.com/DB-CL/cercle/internal/transfer"
	"github.com/DB-CL/cercle/internal/wg"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "pubkey":
		err = pubkey()
	case "version":
		fmt.Println(buildinfo.String())
	default:
		err = fmt.Errorf("sous-commande inconnue %q (serve, pubkey, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cercle :", err)
		os.Exit(1)
	}
}

func pubkey() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	k, err := wg.LoadOrCreateKey(filepath.Join(cfg.DataDir, "node.key"))
	if err != nil {
		return err
	}
	fmt.Println(k.Public())
	return nil
}

func serve() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	key, err := wg.LoadOrCreateKey(filepath.Join(cfg.DataDir, "node.key"))
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "cercle.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := cfg.ApplySettings(func(k string) (string, bool, error) { return db.Setting(ctx, k) }); err != nil {
		return err
	}
	log.Info("démarrage", "version", buildinfo.String(), "node", cfg.NodeName, "public_key", key.Public().String())

	// Toutes les goroutines de fond s'arrêtent avec ctx ; on les attend avant de fermer la base.
	var bg sync.WaitGroup
	defer bg.Wait()
	// Annulé avant bg.Wait (ordre inverse des defer) : une sortie sur erreur ne bloque pas.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ix := index.New(db, cfg.ShareDir, cfg.ShareExclude, log)
	bg.Go(func() { ix.Run(ctx, cfg.ScanInterval) })
	uploads := transfer.NewUploads(cfg.UploadLimit, cfg.MaxUploads, cfg.UploadWindow)

	// J1 : cercle figé par fichier. L'adhésion (J4) remplacera ce bloc.
	if cfg.StaticConfig == "" {
		return errors.New("aucun cercle : en J1, STATIC_CONFIG doit pointer vers un fichier de pairs statiques")
	}
	st, err := config.LoadStatic(cfg.StaticConfig)
	if err != nil {
		return err
	}
	dir, peers, err := staticPeers(ctx, st)
	if err != nil {
		return err
	}

	dev, err := wg.New(wg.Options{PrivateKey: key, ListenPort: cfg.WGListenPort, Address: st.TunnelIP, Logger: log})
	if err != nil {
		return err
	}
	defer dev.Close()
	if err := dev.SetPeers(peers); err != nil {
		return err
	}
	log.Info("tunnel prêt", "tunnel_ip", st.TunnelIP, "listen_port", cfg.WGListenPort, "peers", len(peers))

	l, err := dev.Listen(peerapi.Port)
	if err != nil {
		return err
	}
	srv := &peerapi.Server{
		CircleID:  st.CircleID,
		Directory: dir,
		Info: func(ctx context.Context) peerapi.Info {
			size, files, last := ix.Totals(ctx)
			return peerapi.Info{Name: cfg.NodeName, Version: buildinfo.Version, ShareSize: size, ShareFiles: files, LastScan: last}
		},
		Index:   ix,
		Uploads: uploads,
		Logger:  log.With("module", "peerapi"),
	}
	bg.Go(func() { probe(ctx, log, dev, peerapi.NewClient(dev.DialContext, st.CircleID), dir) })

	err = srv.Serve(ctx, l)
	log.Info("arrêt")
	return err
}

// staticPeers résout les endpoints déclarés et construit l'annuaire.
func staticPeers(ctx context.Context, st config.Static) (staticDir, []wg.Peer, error) {
	dir := staticDir{}
	var peers []wg.Peer
	for _, p := range st.Peers {
		k, err := wg.ParseKey(p.PublicKey)
		if err != nil {
			return nil, nil, fmt.Errorf("pair %q : %w", p.Name, err)
		}
		wp := wg.Peer{PublicKey: k, TunnelIP: p.TunnelIP}
		if p.Endpoint != "" {
			ap, err := resolve(ctx, p.Endpoint)
			if err != nil {
				return nil, nil, fmt.Errorf("pair %q : %w", p.Name, err)
			}
			wp.Endpoint = ap
		}
		peers = append(peers, wp)
		dir[p.TunnelIP] = peerapi.Member{NodeID: k.String(), Name: p.Name, TunnelIP: p.TunnelIP}
	}
	return dir, peers, nil
}

func resolve(ctx context.Context, hostport string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("endpoint %q : %w", hostport, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("résolution de %q : %w", host, err)
	}
	ap, err := netip.ParseAddrPort(net.JoinHostPort(addrs[0].Unmap().String(), port))
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("endpoint %q : %w", hostport, err)
	}
	return ap, nil
}

type staticDir map[netip.Addr]peerapi.Member

func (d staticDir) MemberByIP(ip netip.Addr) (peerapi.Member, bool) {
	m, ok := d[ip]
	return m, ok
}

// probe interroge chaque pair toutes les 30 s et journalise son état. Simple
// témoin de J1, remplacé par le gossip en J4.
func probe(ctx context.Context, log *slog.Logger, dev *wg.Device, c *peerapi.Client, dir staticDir) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		for ip, m := range dir {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			info, err := c.Info(rctx, ip)
			cancel()
			if err != nil {
				log.Info("pair injoignable", "peer", m.Name, "err", err)
				continue
			}
			log.Info("pair en ligne", "peer", m.Name, "name", info.Name, "version", info.Version, "share_files", info.ShareFiles, "share_size", info.ShareSize)
		}
		if st, err := dev.Peers(); err == nil {
			for _, p := range st {
				log.Debug("wireguard", "peer", p.PublicKey.String(), "endpoint", p.Endpoint, "last_handshake", p.LastHandshake, "rx", p.RxBytes, "tx", p.TxBytes)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
