// Commande cercle : partage privé de fichiers entre NAS sur WireGuard.
//
//	cercle serve    démarre le nœud (par défaut)
//	cercle pubkey   affiche la clé publique du nœud (la crée au besoin)
//	cercle version  affiche la version
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	_ "time/tzdata" // l'image scratch n'a pas de zoneinfo

	"github.com/DB-CL/cercle/internal/buildinfo"
	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/node"
	"github.com/DB-CL/cercle/internal/ui"
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

	n, err := node.New(ctx, node.Options{Config: cfg, Logger: log})
	if err != nil {
		return err
	}
	defer n.Close()
	log.Info("démarrage", "version", buildinfo.String(), "node", n.Config().NodeName, "public_key", n.PublicKey().String(), "ui", cfg.UIListen)
	if cfg.UIPassword == "" {
		log.Warn("interface sans mot de passe : définissez UI_PASSWORD si le LAN n'est pas de confiance")
	}

	var wgr sync.WaitGroup
	defer wgr.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	uiSrv := &ui.Server{Node: n, Password: cfg.UIPassword, Logger: log.With("module", "ui")}
	wgr.Go(func() {
		if err := uiSrv.Serve(ctx, cfg.UIListen); err != nil {
			log.Error("interface arrêtée", "err", err)
			cancel()
		}
	})
	err = n.Run(ctx)
	log.Info("arrêt")
	return err
}
