// Command server is the HTTP entrypoint for the zumble-zay backend.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/controlplane"
	"github.com/jackfrancis/zumble-zay/internal/mint"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}

	cp, stopControl, err := buildControlClient(cfg, log)
	if err != nil {
		log.Error("control plane setup failed", "err", err)
		os.Exit(1)
	}
	defer stopControl()

	handler, cleanup := server.New(cfg, log, cp)
	defer cleanup()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("server listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for an interrupt and shut down gracefully.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
}

// loopbackBaseURL returns a URL the process can use to reach its own HTTP
// listener, independent of the externally advertised BaseURL.
func loopbackBaseURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	return "http://" + addr
}

// buildControlClient resolves how the web tier reaches the orchestrator control
// plane (docs/adr/0023). With CONTROL_PLANE_URL set, the orchestrator runs as
// its own Deployment and the web tier calls its control API; this binary then
// holds no Pod-spawning privilege and no Kubernetes client at all. Otherwise the
// orchestrator is co-located in this process behind the in-process launcher — the
// single-process default for local dev, tests, and CI. The returned stop drains
// the co-located orchestrator (a no-op for the remote client).
func buildControlClient(cfg *config.Config, log *slog.Logger) (controlplane.Client, func(), error) {
	if cfg.ControlPlaneURL != "" {
		// The web tier authenticates to the orchestrator with its own projected
		// ServiceAccount token (docs/adr/0031, 0034) — there is no shared-secret
		// fallback, so the token path is required.
		if cfg.ControlPlaneTokenPath == "" {
			return nil, nil, fmt.Errorf("remote control plane requires CONTROL_PLANE_TOKEN_PATH (a projected ServiceAccount token)")
		}
		httpClient := &http.Client{Timeout: 10 * time.Second}
		log.Info("using remote control plane with projected ServiceAccount identity", "url", cfg.ControlPlaneURL)
		return controlplane.NewHTTP(cfg.ControlPlaneURL, httpClient, fileTokenSource(cfg.ControlPlaneTokenPath)), func() {}, nil
	}
	if len(cfg.MintPrivateKey) == 0 {
		return nil, nil, fmt.Errorf("co-located control plane needs a signing key: set CONTROL_PLANE_URL to use a remote orchestrator, or unset MINT_PUBLIC_KEY")
	}
	log.Info("using co-located control plane (in-process orchestrator)")
	minter := mint.NewMinter(cfg.MintPrivateKey, 0)
	launcher := agent.NewInProcessLauncher(loopbackBaseURL(cfg.Addr), &http.Client{Timeout: 30 * time.Second}, log).
		WithAI(cfg.AI.Endpoint, cfg.AI.Model, cfg.AI.Token)
	orch := orchestrator.New(minter, launcher, log)
	return controlplane.NewLocal(orch), orch.Stop, nil
}

// fileTokenSource reads a bearer token from path on each call. A projected
// ServiceAccount token is refreshed in place by kubelet, so re-reading keeps the
// control client's credential current (docs/adr/0031).
func fileTokenSource(path string) func() (string, error) {
	return func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read control token %s: %w", path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
}
