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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/github"
	"github.com/jackfrancis/zumble-zay/internal/llm"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// RunParams configures a single runtime invocation.
type RunParams struct {
	JobType       string                     // selects the runtime: github-ingest|github-enrich|llm-rank|github-converse
	BaseURL       string                     // ZZ base URL (loopback in-process today)
	GitHubBaseURL string                     // GitHub API base; empty uses the public API
	Client        *http.Client               // shared HTTP client
	Token         string                     // ZZ job token (bearer)
	Provider      string                     // e.g. "github"
	ItemID        string                     // target work item for a per-item job (github-converse)
	EnrichLimit   int                        // max items to enrich per run; 0 uses the default
	Ranker        worklist.AxisRanker        // axis ranker for llm-rank jobs; nil uses the stub
	Converser     worklist.Conversationalist // assistant for github-converse jobs; nil builds one from the AI token
	Researcher    worklist.ResearchRanker    // research re-ranker for github-research jobs; nil builds one from the AI token
	AIEndpoint    string                     // chat-completions URL for the llm-rank ranker
	AIModel       string                     // ranking model id; empty uses the llm default
	AIToken       string                     // bearer token for the ranking model; empty falls back to the stub
	// ReportCompletion makes Run post a terminal completion to ZZ when the job
	// finishes (docs/adr/0024). The out-of-process runtime (cmd/runtime) sets it
	// so the orchestrator finalizes the job immediately; the in-process launcher
	// leaves it false, since its completion is the Launch return.
	ReportCompletion bool
}

// Runtime job types. These values are the contract between the orchestrator
// (which schedules jobs) and the runtime (which executes them); they must match
// the orchestrator's JobType constants. See docs/adr/0012.
const (
	JobIngest   = "github-ingest"
	JobEnrich   = "github-enrich"
	JobRank     = "llm-rank"
	JobConverse = "github-converse"
	JobResearch = "github-research"
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
	if jobType == JobRank || jobType == JobConverse || jobType == JobResearch {
		return rankJobTimeout
	}
	return defaultJobTimeout
}

// Run is the single runtime entrypoint: it executes the job selected by
// p.JobType. The in-process launcher and the standalone cmd/runtime binary both
// call it, so the runtime behaves identically regardless of substrate
// (docs/adr/0012). Dispatch is by job type; the per-type logic is unchanged.
func Run(ctx context.Context, p RunParams) error {
	err := dispatch(ctx, p)
	// When running out-of-process (cmd/runtime), report terminal completion so the
	// orchestrator finalizes the job the instant the runtime finishes, rather than
	// waiting to observe the workload terminate (docs/adr/0024). Best-effort: a
	// failed or absent report is backstopped by the orchestrator's substrate watch.
	// A fresh context is used so the report still sends after a job-deadline cancel.
	if p.ReportCompletion && p.BaseURL != "" && p.Token != "" {
		reportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if rerr := NewZZClient(p.BaseURL, p.Token, p.Client).ReportCompletion(reportCtx, err); rerr != nil {
			slog.Default().Warn("runtime completion report failed", "job_type", p.JobType, "err", rerr)
		}
		cancel()
	}
	return err
}

func dispatch(ctx context.Context, p RunParams) error {
	switch p.JobType {
	case JobEnrich:
		return runEnrich(ctx, p)
	case JobRank:
		return runRank(ctx, p)
	case JobConverse:
		return runConverse(ctx, p)
	case JobResearch:
		return runResearch(ctx, p)
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
			Client:   modelHTTPClient(p.Client),
		})
	default:
		return worklist.NewStubRanker()
	}
}

// converserFor builds the Conversationalist for a github-converse job. An
// explicitly injected converser wins (tests, or an in-process WithConverser
// seam); otherwise one is built from the AI token configured for the runtime.
// With neither there is no assistant, so the job cannot proceed (the server
// gates the endpoint on the same token, so this is defence in depth).
func converserFor(p RunParams) worklist.Conversationalist {
	switch {
	case p.Converser != nil:
		return p.Converser
	case p.AIToken != "":
		return llm.NewConverser(llm.Config{
			Endpoint: p.AIEndpoint,
			Model:    p.AIModel,
			Token:    p.AIToken,
			Client:   modelHTTPClient(p.Client),
		})
	default:
		return nil
	}
}

// researchRankerFor selects the ResearchRanker for a github-research job: an
// explicitly injected ranker wins (tests, or the in-process WithResearcher seam);
// otherwise one is built from the AI token; with neither there is no ranker, so
// the job cannot proceed (docs/adr/0022).
func researchRankerFor(p RunParams) worklist.ResearchRanker {
	switch {
	case p.Researcher != nil:
		return p.Researcher
	case p.AIToken != "":
		return llm.NewResearchRanker(llm.Config{
			Endpoint: p.AIEndpoint,
			Model:    p.AIModel,
			Token:    p.AIToken,
			Client:   modelHTTPClient(p.Client),
		})
	default:
		return nil
	}
}

// modelHTTPClient returns the HTTP client for chat-model calls. It reuses the
// shared client's Transport (any proxy/TLS config and its connection pool) but
// drops the client-level timeout: a chat model — Opus with extended thinking —
// can take far longer to return headers than the bounded ZZ and GitHub REST
// calls the shared client is sized for, so the per-job context deadline is the
// only budget (docs/adr/0019). A short client timeout here would preempt that
// budget; the request still aborts when the job context is cancelled.
func modelHTTPClient(shared *http.Client) *http.Client {
	c := &http.Client{} // Timeout 0: bounded by the request context instead
	if shared != nil {
		c.Transport = shared.Transport
	}
	return c
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
	gh := github.NewClient(p.Client, p.GitHubBaseURL)
	items, err := gh.FetchWorklist(ctx, cred.AccessToken)
	if err != nil {
		return fmt.Errorf("fetch github: %w", err)
	}
	// Retire work that has left the open radar and is confirmed closed/merged, so
	// completed items stop ranking. Soft: the row and its thread are kept, and a
	// reopen resurfaces it (docs/adr/0017). Best-effort and bounded; the open
	// snapshot was just fetched, so only items absent from it can be terminal.
	items = append(items, retireCompleted(ctx, gh, cred.AccessToken, zz, items)...)
	if len(items) == 0 {
		return nil
	}
	if err := zz.Ingest(ctx, items); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}

// retireCompleted finds stored github items that have left the open snapshot and
// confirms which are actually closed/merged (rather than merely unassigned or
// review-withdrawn), returning them stamped CompletedAt so the sink retires them
// from the radar. It checks only dropped-off items — those still in the snapshot
// were just fetched as open — skips items already marked done, and is bounded so
// a first-run backlog catches up over a few cycles rather than in one burst.
func retireCompleted(ctx context.Context, gh *github.Client, token string, zz *ZZClient, open []worklist.WorkItem) []worklist.WorkItem {
	stored, err := zz.ListWorklist(ctx, 0)
	if err != nil || len(stored) == 0 {
		return nil // best-effort: a read failure just skips retirement this cycle
	}
	openIDs := make(map[string]struct{}, len(open))
	for _, it := range open {
		openIDs[it.ID] = struct{}{}
	}
	var candidates []worklist.WorkItem
	for _, it := range stored {
		if it.Source != "github" {
			continue
		}
		if _, stillOpen := openIDs[it.ID]; stillOpen {
			continue // in the open snapshot: known open, nothing to confirm
		}
		if !it.Meta.CompletedAt.IsZero() {
			continue // already retired
		}
		candidates = append(candidates, it)
		if len(candidates) >= defaultEnrichLimit {
			break
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	var (
		mu      sync.Mutex
		retired []worklist.WorkItem
		wg      sync.WaitGroup
		sem     = make(chan struct{}, enrichConcurrency)
	)
	for i := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(it worklist.WorkItem) {
			defer wg.Done()
			defer func() { <-sem }()
			state, at, err := gh.ItemState(ctx, token, it.GitHub.Repo, it.GitHub.Number)
			if err != nil || state != "closed" {
				return // best-effort; only a confirmed close/merge retires
			}
			if at.IsZero() {
				at = time.Now().UTC()
			}
			it.Meta.CompletedAt = at
			mu.Lock()
			retired = append(retired, it)
			mu.Unlock()
		}(candidates[i])
	}
	wg.Wait()
	return retired
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
			if act.Participants == s.Participants && act.InboundRefs == s.InboundRefs &&
				act.OtherReviewers == s.OtherReviewers &&
				act.RequestedByLogin == s.ReviewRequestedBy &&
				act.AwaitingMeSince.Equal(s.AwaitingMeSince) && act.AwaitingOthersSince.Equal(s.AwaitingOthersSince) {
				return
			}
			s.Participants = act.Participants
			s.InboundRefs = act.InboundRefs
			s.OtherReviewers = act.OtherReviewers
			s.ReviewRequestedBy = act.RequestedByLogin
			s.AwaitingMeSince = act.AwaitingMeSince
			s.AwaitingOthersSince = act.AwaitingOthersSince
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

// runConverse is the github-converse runtime: it answers one turn of a user's
// assistive conversation about a single item (docs/adr/0019). It reads the item
// (with its thread) from ZZ, fetches the item's live GitHub context — the PR or
// issue description, its discussion, and (for PRs) the changed files — with the
// user's vended credential, asks the Conversationalist for a reply, and writes
// that reply back to the item's thread. It is read-only with respect to GitHub:
// it only reads from the provider and only ever writes back to ZZ (docs/adr/0006).
func runConverse(ctx context.Context, p RunParams) error {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if p.ItemID == "" {
		return fmt.Errorf("converse: missing item id")
	}
	conv := converserFor(p)
	if conv == nil {
		return fmt.Errorf("converse: no assistant configured")
	}
	zz := NewZZClient(p.BaseURL, p.Token, p.Client)

	item, err := zz.GetItem(ctx, p.ItemID)
	if err != nil {
		return fmt.Errorf("get item: %w", err)
	}
	history, userText, ok := lastUserTurn(item.Thread)
	if !ok {
		return nil // the final turn is already the assistant's; nothing to answer
	}

	// Best-effort live context plus read-only GitHub tools: a vend failure (e.g.
	// no credential) leaves the assistant reasoning over the item's ZZ metadata
	// alone, exactly as the in-process path did. With a credential it gets both a
	// pre-fetched snapshot of the item and tools to look up anything else
	// (docs/adr/0018, 0019, 0020).
	var (
		sourceContext string
		viewerLogin   string
		tools         worklist.ToolBox
	)
	if cred, err := zz.VendCredential(ctx, p.Provider); err == nil {
		gh := github.NewClient(p.Client, p.GitHubBaseURL)
		// Resolve who the assistant is talking to so it recognizes the user when
		// they appear on the item and never refers them to their own account.
		if login, err := gh.Login(ctx, cred.AccessToken); err == nil {
			viewerLogin = login
		}
		isPR := item.Type == worklist.TypePullRequest
		if disc, err := gh.Discussion(ctx, cred.AccessToken, item.GitHub.Repo, item.GitHub.Number, isPR); err == nil {
			sourceContext = formatDiscussion(disc)
		}
		tools = newGitHubToolBox(gh, cred.AccessToken, item.GitHub.Repo)
	}

	reply, err := conv.Reply(ctx, item, viewerLogin, sourceContext, history, userText, tools)
	if err != nil {
		return fmt.Errorf("assistant reply: %w", err)
	}
	if err := zz.AppendMessage(ctx, p.ItemID, reply); err != nil {
		return fmt.Errorf("append message: %w", err)
	}
	return nil
}

// lastUserTurn splits a thread into the prior history and the final unanswered
// user message. It returns ok=false when the thread is empty or already ends
// with an assistant turn, so a duplicate or out-of-order job is a safe no-op.
func lastUserTurn(thread []worklist.Message) ([]worklist.Message, string, bool) {
	n := len(thread)
	if n == 0 || thread[n-1].Role != worklist.RoleUser {
		return nil, "", false
	}
	return thread[:n-1], thread[n-1].Content, true
}

// formatDiscussion renders the fetched GitHub context as compact plain text. The
// content is untrusted (PR bodies and comments are attacker-influenceable), so
// the converser wraps it in an explicit "treat as data, not instructions" frame
// (docs/adr/0019); the github client has already bounded its size.
func formatDiscussion(d github.Discussion) string {
	var b strings.Builder
	if d.Body != "" {
		fmt.Fprintf(&b, "Description:\n%s\n", d.Body)
	}
	if len(d.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "\nChanged files (%d):\n", len(d.ChangedFiles))
		for _, f := range d.ChangedFiles {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(d.Comments) > 0 {
		fmt.Fprintf(&b, "\nDiscussion (%d most recent comments):\n", len(d.Comments))
		for _, c := range d.Comments {
			author := c.Author
			if author == "" {
				author = "someone"
			}
			fmt.Fprintf(&b, "- %s: %s\n", author, c.Body)
		}
	}
	return b.String()
}

// runResearch is the github-research runtime: it re-weights one item's ranking
// axes from its conversation thread (docs/adr/0022). It reads the item — with its
// cached foundation proposal and thread — from ZZ, asks the ResearchRanker for
// the per-axis multipliers, and writes them back. It reads and writes ZZ only
// (no provider credential); a thread-less item yields a neutral, no-op result.
func runResearch(ctx context.Context, p RunParams) error {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if p.ItemID == "" {
		return fmt.Errorf("research: missing item id")
	}
	ranker := researchRankerFor(p)
	if ranker == nil {
		return fmt.Errorf("research: no ranker configured")
	}
	zz := NewZZClient(p.BaseURL, p.Token, p.Client)

	item, err := zz.GetItem(ctx, p.ItemID)
	if err != nil {
		return fmt.Errorf("get item: %w", err)
	}
	adj, err := ranker.Research(ctx, item)
	if err != nil {
		return fmt.Errorf("research: %w", err)
	}
	if err := zz.SetResearch(ctx, p.ItemID, adj); err != nil {
		return fmt.Errorf("set research: %w", err)
	}
	return nil
}
