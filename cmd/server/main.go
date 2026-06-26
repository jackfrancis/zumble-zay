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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/k8slauncher"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}

	launcher, err := selectLauncher(cfg, log)
	if err != nil {
		log.Error("launcher setup failed", "err", err)
		os.Exit(1)
	}
	handler, cleanup := server.New(cfg, log, launcher)
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

// selectLauncher builds the agent launcher chosen by the LAUNCHER env var. The
// default in-process launcher reaches ZZ over loopback (docs/adr/0007); the
// k8s-job launcher runs each agent job as a Kubernetes Job (docs/adr/0012).
func selectLauncher(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	switch os.Getenv("LAUNCHER") {
	case "k8s-job":
		restCfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		cs, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, fmt.Errorf("kubernetes client: %w", err)
		}
		lc := k8slauncher.Config{
			Namespace:      envOr("RUNTIME_NAMESPACE", "zumble-zay"),
			Image:          envOr("RUNTIME_IMAGE", "localhost/zumble-zay-runtime:dev"),
			ZZBaseURL:      envOr("RUNTIME_ZZ_BASE_URL", "http://zumble-zay:8080"),
			ServiceAccount: os.Getenv("RUNTIME_SERVICE_ACCOUNT"),
		}
		log.Info("using kubernetes-job launcher", "namespace", lc.Namespace, "image", lc.Image, "zz_base_url", lc.ZZBaseURL)
		return k8slauncher.New(cs, lc), nil
	default:
		log.Info("using in-process launcher")
		return agent.NewInProcessLauncher(loopbackBaseURL(cfg.Addr), &http.Client{Timeout: 30 * time.Second}, log), nil
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
