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

// selectLauncher builds the agent launcher chosen by cfg.Launcher. The default
// in-process launcher reaches ZZ over loopback (docs/adr/0007); the k8s-job
// launcher runs each agent job as a Kubernetes Job (docs/adr/0012). An unknown
// value is a configuration error: selection is the swap mechanism, so a typo or
// an unbuilt substrate must fail fast rather than silently run in-process.
func selectLauncher(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	switch cfg.Launcher {
	case "inprocess":
		log.Info("using in-process launcher")
		return agent.NewInProcessLauncher(loopbackBaseURL(cfg.Addr), &http.Client{Timeout: 30 * time.Second}, log).
			WithAI(cfg.AI.Endpoint, cfg.AI.Model, cfg.AI.Token), nil
	case "k8s-job":
		cs, err := inClusterClientset()
		if err != nil {
			return nil, err
		}
		lc := runtimeLauncherConfig(cfg)
		log.Info("using kubernetes-job launcher", "namespace", lc.Namespace, "image", lc.Image, "zz_base_url", lc.ZZBaseURL)
		return k8slauncher.New(cs, lc), nil
	case "k8s-pod":
		cs, err := inClusterClientset()
		if err != nil {
			return nil, err
		}
		lc := runtimeLauncherConfig(cfg)
		log.Info("using kubernetes-pod launcher", "namespace", lc.Namespace, "image", lc.Image, "zz_base_url", lc.ZZBaseURL)
		return k8slauncher.NewPodLauncher(cs, lc), nil
	default:
		return nil, fmt.Errorf("unknown LAUNCHER %q (want inprocess, k8s-job, or k8s-pod)", cfg.Launcher)
	}
}

// inClusterClientset builds a Kubernetes client from the pod's mounted
// ServiceAccount. It only works inside a cluster, so it is reached only by the
// substrate launchers, never the in-process default.
func inClusterClientset() (*kubernetes.Clientset, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return cs, nil
}

// runtimeLauncherConfig maps the runtime settings onto the substrate launcher
// config shared by the Job and Pod launchers.
func runtimeLauncherConfig(cfg *config.Config) k8slauncher.Config {
	return k8slauncher.Config{
		Namespace:         cfg.Runtime.Namespace,
		Image:             cfg.Runtime.Image,
		ZZBaseURL:         cfg.Runtime.ZZBaseURL,
		ServiceAccount:    cfg.Runtime.ServiceAccount,
		AIEndpoint:        cfg.AI.Endpoint,
		AIModel:           cfg.AI.Model,
		AITokenSecretName: cfg.AI.TokenSecretName,
		AITokenSecretKey:  cfg.AI.TokenSecretKey,
	}
}
