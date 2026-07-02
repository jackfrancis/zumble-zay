// Command orchestrator is ZZ's control plane for agent runtimes (docs/adr/0002,
// 0007, 0023). It was extracted from the web tier so that Pod/Job-creation
// privilege and the Kubernetes client live only here, never in the
// internet-facing process. It is the sole issuer of job tokens (it holds the
// Ed25519 private key) and serves a small, bearer-authenticated control API the
// web tier calls to trigger agent work: backfill a worklist, answer a
// conversation turn, or re-rank an item.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/controlauth"
	"github.com/jackfrancis/zumble-zay/internal/controlplane"
	"github.com/jackfrancis/zumble-zay/internal/k8slauncher"
	"github.com/jackfrancis/zumble-zay/internal/launcher"
	"github.com/jackfrancis/zumble-zay/internal/mint"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}

	// The orchestrator is the sole token issuer, so it must hold a private key
	// (docs/adr/0023). A verify-only configuration (MINT_PUBLIC_KEY) is for the
	// web tier and cannot mint.
	if len(cfg.MintPrivateKey) == 0 {
		log.Error("orchestrator requires a signing key: set MINT_PRIVATE_KEY, or SESSION_SECRET without MINT_PUBLIC_KEY")
		os.Exit(1)
	}
	// The control API triggers privileged spawns, so it must be authenticated.
	// Fail closed rather than serve it open.
	if len(cfg.ControlPlaneToken) == 0 {
		log.Error("orchestrator requires CONTROL_PLANE_TOKEN to authenticate the control API")
		os.Exit(1)
	}

	agentLauncher, err := launcher.Build(cfg, log)
	if err != nil {
		log.Error("launcher setup failed", "err", err)
		os.Exit(1)
	}

	minter := mint.NewMinter(cfg.MintPrivateKey, 0)
	orch := orchestrator.New(minter, agentLauncher, log)
	defer orch.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// The control API: the web tier's trigger routes plus the token-exchange
	// endpoint (docs/adr/0024). Every route is authenticated through the handler's
	// CallerAuthenticator. With CONTROL_PLANE_AUDIENCE set, that is per-service
	// Kubernetes workload identity — a projected ServiceAccount token validated by
	// TokenReview — chained over the shared bearer as a migration fallback
	// (docs/adr/0031); otherwise it is the shared bearer alone.
	control := controlplane.NewHandler(orch, cfg.ControlPlaneToken, log)
	if cfg.ControlPlaneAudience != "" {
		tr, err := controlauth.Build(cfg.ControlPlaneAudience, cfg.ControlPlaneCallers, log)
		if err != nil {
			log.Error("control-plane caller identity setup failed", "err", err)
			os.Exit(1)
		}
		control = control.WithCaller(controlplane.NewChainCallerAuthenticator(
			tr, controlplane.NewBearerCallerAuthenticator(cfg.ControlPlaneToken)))
		log.Info("control API: per-service caller identity enabled", "audience", cfg.ControlPlaneAudience)
	}
	control.WithTokenExchange(orch).Register(mux)

	srv := &http.Server{
		Addr:              cfg.ControlPlaneAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("orchestrator control API listening", "addr", cfg.ControlPlaneAddr, "launcher", cfg.Launcher)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("control API failed", "err", err)
			os.Exit(1)
		}
	}()

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

// The launchers ZZ ships with register themselves here (docs/adr/0024). To add a
// substrate — kagent, agent-sandbox, Ray, … — implement orchestrator.Launcher
// (optionally orchestrator.AsyncLauncher), call launcher.Register from that
// package's init, and add a blank import for it alongside these; nothing in this
// file's dispatch path changes. TODO(team): non-native substrates live behind
// this seam and are intentionally not implemented yet.
func init() {
	launcher.Register("inprocess", inProcessLauncher)
	launcher.Register("k8s-job", k8sJobLauncher)
	launcher.Register("k8s-pod", k8sPodLauncher)
	launcher.Register("k8s-pod-detached", k8sDetachedPodLauncher)
}

// inProcessLauncher runs the agent in this process, dialing the web tier at
// RUNTIME_ZZ_BASE_URL (not loopback: the worklist store and credential vault
// live in the web tier, docs/adr/0023).
func inProcessLauncher(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	log.Info("using in-process launcher", "zz_base_url", cfg.Runtime.ZZBaseURL)
	return agent.NewInProcessLauncher(cfg.Runtime.ZZBaseURL, &http.Client{Timeout: 30 * time.Second}, log).
		WithAI(cfg.AI.Endpoint, cfg.AI.Model, cfg.AI.Token), nil
}

// k8sJobLauncher runs each agent job as a Kubernetes Job (docs/adr/0012).
func k8sJobLauncher(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	cs, err := inClusterClientset()
	if err != nil {
		return nil, err
	}
	lc := runtimeLauncherConfig(cfg)
	log.Info("using kubernetes-job launcher", "namespace", lc.Namespace, "image", lc.Image, "zz_base_url", lc.ZZBaseURL)
	return k8slauncher.New(cs, lc), nil
}

// k8sPodLauncher runs each agent job as a bare Pod (docs/adr/0012).
func k8sPodLauncher(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	cs, err := inClusterClientset()
	if err != nil {
		return nil, err
	}
	lc := runtimeLauncherConfig(cfg)
	log.Info("using kubernetes-pod launcher", "namespace", lc.Namespace, "image", lc.Image, "zz_base_url", lc.ZZBaseURL)
	return k8slauncher.NewPodLauncher(cs, lc), nil
}

// k8sDetachedPodLauncher runs each agent job as a bare Pod but does NOT watch it:
// completion arrives solely from the runtime's callback (docs/adr/0025), with the
// per-job deadline as the only backstop. It is the in-cluster reference for a
// fully-detached substrate and needs only Pod-create RBAC, not get/list/watch.
func k8sDetachedPodLauncher(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	cs, err := inClusterClientset()
	if err != nil {
		return nil, err
	}
	lc := runtimeLauncherConfig(cfg)
	log.Info("using kubernetes-pod launcher (detached: completion via callback only)", "namespace", lc.Namespace, "image", lc.Image, "zz_base_url", lc.ZZBaseURL)
	return k8slauncher.NewDetachedPodLauncher(cs, lc), nil
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
