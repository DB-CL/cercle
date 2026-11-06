// Command cercle: private file sharing between NAS over WireGuard.
//
//	cercle serve        starts the node (default)
//	cercle healthcheck  queries the local interface (image HEALTHCHECK, no curl)
//	cercle pubkey       prints the node's public key (creating it if needed)
//	cercle version      prints the version
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
	_ "time/tzdata" // the scratch image has no zoneinfo

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
		err = fmt.Errorf("unknown subcommand %q (serve, healthcheck, pubkey, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cercle:", err)
		os.Exit(1)
	}
}

// healthcheck queries GET /healthz on the local interface: 0 if it answers.
func healthcheck() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.UIListen)
	if err != nil {
		return fmt.Errorf("unreadable UI_LISTEN: %w", err)
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
		return fmt.Errorf("interface: %s", resp.Status)
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
	log.Info("starting", "version", buildinfo.String(), "node", n.Config().NodeName, "public_key", n.PublicKey().String(), "ui", cfg.UIListen)
	password := cfg.UIPassword
	if password == "" {
		pwFile := filepath.Join(cfg.DataDir, "ui.password")
		var created bool
		if password, created, err = ui.LoadOrCreatePassword(pwFile); err != nil {
			return err
		}
		if created {
			log.Warn("UI_PASSWORD not set: random password created, read it in the ui.password file of the /data volume", "file", pwFile)
		} else {
			log.Info("UI_PASSWORD not set: password read from the ui.password file of the /data volume", "file", pwFile)
		}
	}

	var wgr sync.WaitGroup
	defer wgr.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	uiSrv := &ui.Server{Node: n, Password: password, Logger: log.With("module", "ui")}
	wgr.Go(func() {
		if err := uiSrv.Serve(ctx, cfg.UIListen); err != nil {
			log.Error("interface stopped", "err", err)
			cancel()
		}
	})
	err = n.Run(ctx)
	log.Info("stopping")
	return err
}
