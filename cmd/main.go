// mint4v authenticates to Vault with its Kubernetes
// ServiceAccount token, pushes the resulting short-lived Vault token to a
// target API (e.g. Cloud Pak for Data), and manages renewal, rotation, and
// revocation of that token for the life of the process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hashicorp/vault/api"

	"mint4v/internal/config"
	"mint4v/internal/pusher"
	"mint4v/internal/vaulttoken"
)

func main() {
	configPath := flag.String("config", "", "path to the HCL config file (required)")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: mint4v -config /path/to/config.hcl")
		os.Exit(2)
	}

	if err := run(*configPath); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	client, err := newVaultClient(cfg.Vault)
	if err != nil {
		return err
	}

	p, err := pusher.New(cfg.Push, logger)
	if err != nil {
		return err
	}

	manager := vaulttoken.NewManager(client, cfg.Vault.Auth, cfg.Vault.RevokeGraceDuration(), p.Push, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		ok, reason := manager.Healthy()
		if !ok {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, reason)
	})
	health := &http.Server{Addr: cfg.HealthAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := health.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server failed", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	err = manager.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = health.Shutdown(shutdownCtx)
	return err
}

func newVaultClient(cfg config.Vault) (*api.Client, error) {
	vc := api.DefaultConfig()
	vc.Address = cfg.Address
	if cfg.CACertFile != "" {
		if err := vc.ConfigureTLS(&api.TLSConfig{CACert: cfg.CACertFile}); err != nil {
			return nil, fmt.Errorf("vault.ca_cert_file: %w", err)
		}
	}
	client, err := api.NewClient(vc)
	if err != nil {
		return nil, err
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}
	return client, nil
}
