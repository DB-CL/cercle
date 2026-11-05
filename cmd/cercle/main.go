// Commande cercle : partage privé de fichiers entre NAS sur WireGuard.
//
//	cercle serve        démarre le nœud (par défaut)
//	cercle healthcheck  interroge l'interface locale (HEALTHCHECK de l'image, sans curl)
//	cercle pubkey       affiche la clé publique du nœud (la crée au besoin)
//	cercle version      affiche la version
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
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
	case "healthcheck":
		err = healthcheck()
	case "pubkey":
		err = pubkey()
	case "version":
		fmt.Println(buildinfo.String())
	default:
		err = fmt.Errorf("sous-commande inconnue %q (serve, healthcheck, pubkey, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cercle :", err)
		os.Exit(1)
	}
}

// healthcheck interroge GET /healthz de l'interface locale : 0 si elle répond.
func healthcheck() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.UIListen)
	if err != nil {
		return fmt.Errorf("UI_LISTEN illisible : %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("interface : %s", resp.Status)
	}
	return nil
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
	password := cfg.UIPassword
	if password == "" {
		pwFile := filepath.Join(cfg.DataDir, "ui.password")
		var created bool
		if password, created, err = ui.LoadOrCreatePassword(pwFile); err != nil {
			return err
		}
		if created {
			log.Warn("UI_PASSWORD absent : mot de passe aléatoire créé, à lire dans le fichier ui.password du volume /data", "file", pwFile)
		} else {
			log.Info("UI_PASSWORD absent : mot de passe lu dans le fichier ui.password du volume /data", "file", pwFile)
		}
	}

	var wgr sync.WaitGroup
	defer wgr.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	uiSrv := &ui.Server{Node: n, Password: password, Logger: log.With("module", "ui")}
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
