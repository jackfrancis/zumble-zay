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
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// RunParams configures a single runtime invocation.
type RunParams struct {
	BaseURL       string       // ZZ base URL (loopback in-process today)
	GitHubBaseURL string       // GitHub API base; empty uses the public API
	Client        *http.Client // shared HTTP client
	Token         string       // ZZ job token (bearer)
	Provider      string       // e.g. "github"
	EnrichLimit   int          // max items to enrich per run; 0 uses the default
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

// defaultEnrichLimit bounds how many items a single enrich run fetches expensive
// per-item signals for, capping GitHub API fan-out (docs/adr/0010).
const defaultEnrichLimit = 50

func enrichLimit(n int) int {
	if n <= 0 {
		return defaultEnrichLimit
	}
	return n
}

// RunEnrich is the github-enrich runtime: it reads the user's persisted work
// from ZZ and augments the review-requested PRs with the AwaitingMeSince signal
// (how long each has been blocked on the user), writing back only the items it
// changed. Like Run it speaks only the ZZClient contract and connects to GitHub
// directly (docs/adr/0006, 0009, 0010). Per-item enrichment is best-effort: a
// failed call leaves the signal unchanged.
func RunEnrich(ctx context.Context, p RunParams) error {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 30 * time.Second}
	}
	zz := NewZZClient(p.BaseURL, p.Token, p.Client)

	cred, err := zz.VendCredential(ctx, p.Provider)
	if err != nil {
		return fmt.Errorf("vend credential: %w", err)
	}
	gh := github.NewClient(p.Client, p.GitHubBaseURL)
	login, err := gh.Login(ctx, cred.AccessToken)
	if err != nil {
		return fmt.Errorf("github login: %w", err)
	}
	items, err := zz.ListWorklist(ctx, enrichLimit(p.EnrichLimit))
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	// Augment stored items in place from a single per-item timeline call, and
	// write back only what changed (docs/adr/0010).
	var changed []worklist.WorkItem
	for i := range items {
		act, err := gh.ItemActivity(ctx, cred.AccessToken, items[i].GitHub.Repo, items[i].GitHub.Number, login)
		if err != nil {
			continue // best-effort: leave the item's signals unchanged
		}
		s := &items[i].Signals
		if act.Participants == s.Participants && act.InboundRefs == s.InboundRefs && act.AwaitingMeSince.Equal(s.AwaitingMeSince) {
			continue
		}
		s.Participants = act.Participants
		s.InboundRefs = act.InboundRefs
		s.AwaitingMeSince = act.AwaitingMeSince
		changed = append(changed, items[i])
	}
	if len(changed) == 0 {
		return nil
	}
	if err := zz.Ingest(ctx, changed); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
