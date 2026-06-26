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
	"strconv"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	p := agent.RunParams{
		JobType:       os.Getenv("ZZ_JOB_TYPE"),
		BaseURL:       os.Getenv("ZZ_BASE_URL"),
		Token:         os.Getenv("ZZ_JOB_TOKEN"),
		Provider:      os.Getenv("ZZ_PROVIDER"),
		GitHubBaseURL: os.Getenv("ZZ_GITHUB_BASE_URL"),
	}
	if v := os.Getenv("ZZ_ENRICH_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.EnrichLimit = n
		}
	}
	if p.BaseURL == "" || p.Token == "" || p.JobType == "" {
		log.Error("missing required runtime configuration",
			"need", "ZZ_BASE_URL, ZZ_JOB_TOKEN, ZZ_JOB_TYPE")
		os.Exit(2)
	}

	log.Info("runtime starting", "job_type", p.JobType, "provider", p.Provider, "base_url", p.BaseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := agent.Run(ctx, p); err != nil {
		log.Error("runtime failed", "job_type", p.JobType, "err", err)
		os.Exit(1)
	}
	log.Info("runtime succeeded", "job_type", p.JobType)
}
