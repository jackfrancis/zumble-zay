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
	converser     worklist.Conversationalist
	researcher    worklist.ResearchRanker
	aiEndpoint    string
	aiModel       string
	aiToken       string
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
// default) makes the llm-rank runtime use the deterministic StubRanker.
func (l *InProcessLauncher) WithRanker(r worklist.AxisRanker) *InProcessLauncher {
	l.ranker = r
	return l
}

// WithConverser sets the Conversationalist used by github-converse jobs. A nil
// converser (the default) makes the runtime build one from the configured AI
// token instead.
func (l *InProcessLauncher) WithConverser(c worklist.Conversationalist) *InProcessLauncher {
	l.converser = c
	return l
}

// WithResearcher sets the ResearchRanker used by github-research jobs. A nil
// researcher (the default) makes the runtime build one from the configured AI
// token instead.
func (l *InProcessLauncher) WithResearcher(r worklist.ResearchRanker) *InProcessLauncher {
	l.researcher = r
	return l
}

// WithAI configures the chat-model ranker for llm-rank jobs. When the token is
// non-empty and no explicit ranker is set, the runtime builds a chat-model
// ranker from these values; otherwise it falls back to the StubRanker.
func (l *InProcessLauncher) WithAI(endpoint, model, token string) *InProcessLauncher {
	l.aiEndpoint, l.aiModel, l.aiToken = endpoint, model, token
	return l
}

// Launch satisfies orchestrator.Launcher by running the runtime to completion.
// It drives the same single runtime entrypoint (agent.Run) the standalone
// cmd/runtime binary uses, so behaviour is identical across substrates; job-type
// dispatch lives in agent.Run (docs/adr/0012).
func (l *InProcessLauncher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	if l.log != nil {
		l.log.Info("agent runtime starting", "job", spec.JobID, "type", spec.Type, "provider", spec.Provider)
	}
	err := Run(ctx, RunParams{
		JobType:       string(spec.Type),
		BaseURL:       l.zzBaseURL,
		GitHubBaseURL: l.githubBaseURL,
		Client:        l.client,
		Token:         token,
		Provider:      spec.Provider,
		ItemID:        spec.ItemID,
		Ranker:        l.ranker,
		Converser:     l.converser,
		Researcher:    l.researcher,
		AIEndpoint:    l.aiEndpoint,
		AIModel:       l.aiModel,
		AIToken:       l.aiToken,
	})
	return orchestrator.Handle{Kind: "inprocess"}, err
}
