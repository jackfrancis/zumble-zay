// Package agent is the ephemeral agent runtime. Its single entrypoint, Run,
// dispatches on job type (github-ingest, github-enrich, llm-rank) and is shared
// by the in-process launcher and the standalone cmd/runtime binary
// (docs/adr/0012). It is the only component besides the composition root that
// imports a provider client: ZZ is a credential broker, not a data broker, so
// the agent connects to GitHub directly (see docs/adr/0006).
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
	"sync"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/github"
	"github.com/jackfrancis/zumble-zay/internal/llm"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// RunParams configures a single runtime invocation.
type RunParams struct {
	JobType       string              // selects the runtime: github-ingest|github-enrich|llm-rank
	BaseURL       string              // ZZ base URL (loopback in-process today)
	GitHubBaseURL string              // GitHub API base; empty uses the public API
	Client        *http.Client        // shared HTTP client
	Token         string              // ZZ job token (bearer)
	Provider      string              // e.g. "github"
	EnrichLimit   int                 // max items to enrich per run; 0 uses the default
	Ranker        worklist.AxisRanker // axis ranker for llm-rank jobs; nil uses the stub
	AIEndpoint    string              // chat-completions URL for the llm-rank ranker
	AIModel       string              // ranking model id; empty uses the llm default
	AIToken       string              // bearer token for the ranking model; empty falls back to the stub
}

// Runtime job types. These values are the contract between the orchestrator
// (which schedules jobs) and the runtime (which executes them); they must match
// the orchestrator's JobType constants. See docs/adr/0012.
const (
	JobIngest = "github-ingest"
	JobEnrich = "github-enrich"
	JobRank   = "llm-rank"
)

// Runtime job budgets. The llm-rank job makes one slow chat-model call per
// shortlisted item, so it needs more wall clock than the bounded GitHub stages.
const (
	defaultJobTimeout = 2 * time.Minute
	rankJobTimeout    = 5 * time.Minute
)

// JobTimeout returns the wall-clock budget a standalone runtime should allow for
// a job of the given type (cmd/runtime applies it). The in-process path is
// bounded by the orchestrator's per-stage deadline instead; the two are kept in
// step so a job has the same budget on either substrate.
func JobTimeout(jobType string) time.Duration {
	if jobType == JobRank {
		return rankJobTimeout
	}
	return defaultJobTimeout
}

// Run is the single runtime entrypoint: it executes the job selected by
// p.JobType. The in-process launcher and the standalone cmd/runtime binary both
// call it, so the runtime behaves identically regardless of substrate
// (docs/adr/0012). Dispatch is by job type; the per-type logic is unchanged.
func Run(ctx context.Context, p RunParams) error {
	switch p.JobType {
	case JobEnrich:
		return runEnrich(ctx, p)
	case JobRank:
		return runRank(ctx, p)
	case JobIngest:
		return runIngest(ctx, p)
	default:
		return fmt.Errorf("agent: unknown job type %q", p.JobType)
	}
}

// rankerFor selects the AxisRanker for a rank job: an explicitly injected ranker
// wins (tests, or the in-process WithRanker seam); otherwise a chat-model ranker
// is built when an AI token is configured; otherwise the deterministic stub
// keeps the pipeline exercisable with no model attached (docs/adr/0011).
func rankerFor(p RunParams) worklist.AxisRanker {
	switch {
	case p.Ranker != nil:
		return p.Ranker
	case p.AIToken != "":
		return llm.NewRanker(llm.Config{
			Endpoint: p.AIEndpoint,
			Model:    p.AIModel,
			Token:    p.AIToken,
			Client:   p.Client,
		})
	default:
		return worklist.NewStubRanker()
	}
}

// runIngest executes the github-ingest job: vend the provider credential from
// ZZ, fetch the user's work directly from GitHub, then post it to ZZ's ingest
// sink. An empty result is a successful no-op. The ZZ calls go through ZZClient,
// the substrate-neutral runtime contract (docs/adr/0009).
func runIngest(ctx context.Context, p RunParams) error {
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

// enrichConcurrency bounds how many per-item timeline calls run at once, so the
// fan-out finishes quickly without overrunning the job deadline.
const enrichConcurrency = 8

// rankConcurrency bounds how many per-item ranking calls run at once. A chat
// model is slow (seconds per call, more with adaptive thinking), so ranking the
// shortlist sequentially would exceed the job deadline; this fans the calls out
// while staying friendly to provider rate limits.
const rankConcurrency = 8

func enrichLimit(n int) int {
	if n <= 0 {
		return defaultEnrichLimit
	}
	return n
}

// runEnrich is the github-enrich runtime: it reads the user's persisted work
// from ZZ and augments the review-requested PRs with the AwaitingMeSince signal
// (how long each has been blocked on the user), writing back only the items it
// changed. Like runIngest it speaks only the ZZClient contract and connects to
// GitHub directly (docs/adr/0006, 0009, 0010). Per-item enrichment is
// best-effort: a failed call leaves the signal unchanged.
func runEnrich(ctx context.Context, p RunParams) error {
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
	// Augment stored items in place from a per-item timeline call. The calls run
	// with bounded concurrency so the fan-out finishes quickly and does not blow
	// the job deadline (docs/adr/0010). Only changed items are written back.
	var (
		mu      sync.Mutex
		changed []worklist.WorkItem
		wg      sync.WaitGroup
		sem     = make(chan struct{}, enrichConcurrency)
	)
	for i := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			act, err := gh.ItemActivity(ctx, cred.AccessToken, items[i].GitHub.Repo, items[i].GitHub.Number, login)
			if err != nil {
				return // best-effort: leave the item's signals unchanged
			}
			s := &items[i].Signals
			if act.Participants == s.Participants && act.InboundRefs == s.InboundRefs && act.AwaitingMeSince.Equal(s.AwaitingMeSince) {
				return
			}
			s.Participants = act.Participants
			s.InboundRefs = act.InboundRefs
			s.AwaitingMeSince = act.AwaitingMeSince
			mu.Lock()
			changed = append(changed, items[i])
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if len(changed) == 0 {
		return nil
	}
	if err := zz.Ingest(ctx, changed); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}

// runRank is the llm-rank runtime: it reads the user's persisted work (the
// ranked top-K shortlist), asks the AxisRanker to propose the four axes for each
// item, and writes the proposals back to ZZ, which ratifies them against the
// deterministic baseline (docs/adr/0011). With the StubRanker this is a no-op
// over ordering; a real model is swapped in behind the AxisRanker interface.
// Per-item ranking is best-effort: a failed proposal leaves the item unchanged.
func runRank(ctx context.Context, p RunParams) error {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 30 * time.Second}
	}
	ranker := rankerFor(p)
	zz := NewZZClient(p.BaseURL, p.Token, p.Client)

	items, err := zz.ListWorklist(ctx, enrichLimit(p.EnrichLimit))
	if err != nil {
		return fmt.Errorf("list worklist: %w", err)
	}
	// Propose axes per item with bounded concurrency: a chat model is slow, so
	// ranking the shortlist one item at a time would exceed the job deadline
	// (the failure mode the sequential version hit). Only items that received a
	// proposal are written back; a failed call leaves the item unchanged.
	var (
		mu      sync.Mutex
		changed []worklist.WorkItem
		wg      sync.WaitGroup
		sem     = make(chan struct{}, rankConcurrency)
	)
	for i := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			prop, err := ranker.Propose(ctx, items[i])
			if err != nil {
				return // best-effort: leave the item without a proposal
			}
			items[i].Signals.Proposed = &prop
			mu.Lock()
			changed = append(changed, items[i])
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if len(changed) == 0 {
		return nil
	}
	if err := zz.Ingest(ctx, changed); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
