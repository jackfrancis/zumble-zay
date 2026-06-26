package agent

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// InProcessLauncher runs an agent runtime inline in the ZZ process. It is the
// co-located substrate from docs/adr/0007: the runtime uses the same HTTP
// contract (vend credential, then ingest) a future out-of-process Pod will use,
// so swapping to a Kubernetes launcher does not change the runtime.
type InProcessLauncher struct {
	zzBaseURL     string
	githubBaseURL string
	client        *http.Client
	log           *slog.Logger
	ranker        worklist.AxisRanker
}

// NewInProcessLauncher builds a launcher that targets ZZ at zzBaseURL (a
// loopback address in-process). A nil client gets a sane default.
func NewInProcessLauncher(zzBaseURL string, client *http.Client, log *slog.Logger) *InProcessLauncher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &InProcessLauncher{zzBaseURL: zzBaseURL, client: client, log: log}
}

// WithGitHubBaseURL overrides the GitHub API base URL (tests point it at a
// stub). It returns the launcher for chaining and must be set before any job
// runs.
func (l *InProcessLauncher) WithGitHubBaseURL(u string) *InProcessLauncher {
	l.githubBaseURL = u
	return l
}

// WithRanker sets the AxisRanker used by llm-rank jobs. A nil ranker (the
// default) makes RunRank use the deterministic StubRanker.
func (l *InProcessLauncher) WithRanker(r worklist.AxisRanker) *InProcessLauncher {
	l.ranker = r
	return l
}

// Launch satisfies orchestrator.Launcher by running the runtime to completion.
// It selects the runtime entrypoint by job type, so each capability (ingest,
// enrich, llm-rank) is a distinct unit behind the same dispatch seam.
func (l *InProcessLauncher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) error {
	if l.log != nil {
		l.log.Info("agent runtime starting", "job", spec.JobID, "type", spec.Type, "provider", spec.Provider)
	}
	p := RunParams{
		BaseURL:       l.zzBaseURL,
		GitHubBaseURL: l.githubBaseURL,
		Client:        l.client,
		Token:         token,
		Provider:      spec.Provider,
		Ranker:        l.ranker,
	}
	switch spec.Type {
	case orchestrator.JobGitHubEnrich:
		return RunEnrich(ctx, p)
	case orchestrator.JobLLMRank:
		return RunRank(ctx, p)
	default:
		return Run(ctx, p)
	}
}
