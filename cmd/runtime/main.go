// Command runtime is the standalone agent runtime. It reads its job from the
// environment — the injection contract of docs/adr/0012 — and executes it
// against ZZ over the ZZClient HTTP contract. The same logic runs in-process via
// agent.InProcessLauncher; this binary lets an identical runtime run as an
// out-of-process workload (a Kubernetes Job/Pod, a sandbox, etc.).
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackfrancis/zumble-zay/internal/agent"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	p, err := agent.ParamsFromEnv(os.Getenv)
	if err != nil {
		log.Error("invalid runtime configuration", "err", err)
		os.Exit(2)
	}

	log.Info("runtime starting", "job_type", p.JobType, "provider", p.Provider, "base_url", p.BaseURL)

	ctx, cancel := context.WithTimeout(context.Background(), agent.JobTimeout(p.JobType))
	defer cancel()
	if err := agent.Run(ctx, p); err != nil {
		log.Error("runtime failed", "job_type", p.JobType, "err", err)
		os.Exit(1)
	}
	log.Info("runtime succeeded", "job_type", p.JobType)
}
