// Package agent is the ephemeral GitHub-ingestion runtime. It is the only
// component besides the composition root that imports a provider client: ZZ is
// a credential broker, not a data broker, so the agent connects to GitHub
// directly (see docs/adr/0006).
//
// A runtime carries a ZZ-minted job token and uses the same HTTP contract a
// future out-of-process Pod will use (docs/adr/0007): it vends the user's
// provider credential from ZZ, calls GitHub directly, and posts results back to
// ZZ's ingest sink. It never sees the user's raw token until ZZ vends it, and
// never writes anywhere but ZZ.
package agent

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/github"
)

// RunParams configures a single runtime invocation.
type RunParams struct {
	BaseURL       string       // ZZ base URL (loopback in-process today)
	GitHubBaseURL string       // GitHub API base; empty uses the public API
	Client        *http.Client // shared HTTP client
	Token         string       // ZZ job token (bearer)
	Provider      string       // e.g. "github"
}

// Run executes the ingestion job: vend the provider credential from ZZ, fetch
// the user's work directly from GitHub, then post it to ZZ's ingest sink. An
// empty result is a successful no-op. The ZZ calls go through ZZClient, the
// substrate-neutral runtime contract (docs/adr/0009).
func Run(ctx context.Context, p RunParams) error {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 30 * time.Second}
	}
	zz := NewZZClient(p.BaseURL, p.Token, p.Client)

	cred, err := zz.VendCredential(ctx, p.Provider)
	if err != nil {
		return fmt.Errorf("vend credential: %w", err)
	}
	items, err := github.NewClient(p.Client, p.GitHubBaseURL).FetchWorklist(ctx, cred.AccessToken)
	if err != nil {
		return fmt.Errorf("fetch github: %w", err)
	}
	if len(items) == 0 {
		return nil
	}
	if err := zz.Ingest(ctx, items); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
