package orchestrator_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/mint"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/runtimestats"
)

const testSecret = "test-secret-of-sufficient-length!"

// blockingLauncher signals each Launch entry and blocks until released, so a
// job can be held in the "running" state deterministically.
type blockingLauncher struct {
	started chan orchestrator.JobSpec
	release chan struct{}

	mu     sync.Mutex
	tokens []string
}

func (b *blockingLauncher) Launch(_ context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	b.mu.Lock()
	b.tokens = append(b.tokens, token)
	b.mu.Unlock()
	b.started <- spec
	<-b.release
	return orchestrator.Handle{Kind: "blocking", Ref: spec.JobID}, nil
}

func (b *blockingLauncher) lastToken() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens[len(b.tokens)-1]
}

// pullLauncher is a PullTokenLauncher: it captures the credential the
// orchestrator hands it, which for a pull substrate is a single-use redemption
// ticket rather than the job token (docs/adr/0029).
type pullLauncher struct{ creds chan string }

func (p *pullLauncher) Launch(_ context.Context, spec orchestrator.JobSpec, cred string) (orchestrator.Handle, error) {
	select {
	case p.creds <- cred: // capture the first; later chained stages are dropped
	default:
	}
	return orchestrator.Handle{Kind: "pull", Ref: spec.JobID}, nil
}

func (p *pullLauncher) PullsToken() bool { return true }

func TestPullLauncherGetsSingleUseTicketNotToken(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	pl := &pullLauncher{creds: make(chan string, 1)}
	o := orchestrator.New(m, pl, nil)
	defer o.Stop()

	if err := o.EnsureBackfill(context.Background(), "github:1"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	var ticket string
	select {
	case ticket = <-pl.creds:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not start")
	}

	// A pull substrate must NOT receive a usable job token: the credential handed
	// to it is a ticket, which is not a signed token and does not verify.
	if _, err := m.Verifier().Verify(ticket); err == nil {
		t.Fatal("pull launcher must receive a ticket, not a verifiable job token")
	}

	// Redeeming yields the real job token, scoped to the acting user and carrying
	// the dispatched job's id so the completion callback still correlates.
	tok, ttl, err := o.RedeemTicket(ticket)
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}
	if ttl <= 0 {
		t.Errorf("ttl = %v, want positive", ttl)
	}
	claims, err := m.Verifier().Verify(tok)
	if err != nil {
		t.Fatalf("verify redeemed token: %v", err)
	}
	if claims.ActingUserID != "github:1" || claims.Provider != "github" || claims.JobID == "" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if !hasScope(claims.Scopes, principal.ScopeSignalsRead) {
		t.Fatalf("redeemed token missing scopes: %+v", claims.Scopes)
	}

	// Single-use: a second redemption of the same ticket fails.
	if _, _, err := o.RedeemTicket(ticket); err == nil {
		t.Fatal("ticket must be single-use; a second redemption should fail")
	}
}

func TestRedeemUnknownTicketFails(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	o := orchestrator.New(m, orchestrator.NoopLauncher{}, nil)
	defer o.Stop()
	if _, _, err := o.RedeemTicket("nope"); err == nil {
		t.Fatal("redeeming an unknown ticket must fail")
	}
}

func TestEnsureBackfillDedupesAndMintsScopedToken(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	fl := &blockingLauncher{started: make(chan orchestrator.JobSpec, 4), release: make(chan struct{})}
	o := orchestrator.New(m, fl, nil)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fl.release) }) }
	// Cleanup runs LIFO: release unblocks any held runtime before Stop drains,
	// so a failed assertion cannot hang the worker pool.
	defer o.Stop()
	defer release()

	if err := o.EnsureBackfill(context.Background(), "github:1"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}

	var spec orchestrator.JobSpec
	select {
	case spec = <-fl.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not start")
	}
	if spec.ActingUserID != "github:1" || spec.Provider != "github" {
		t.Fatalf("unexpected spec: %+v", spec)
	}

	// A second request for the same user while the first runs is deduped.
	if err := o.EnsureBackfill(context.Background(), "github:1"); err != nil {
		t.Fatalf("second EnsureBackfill: %v", err)
	}
	select {
	case <-fl.started:
		t.Fatal("expected dedupe, but a second runtime started")
	case <-time.After(150 * time.Millisecond):
	}

	// The minted token is workload-scoped to the acting user and job provider.
	claims, err := m.Verifier().Verify(fl.lastToken())
	if err != nil {
		t.Fatalf("verify minted token: %v", err)
	}
	if claims.ActingUserID != "github:1" || claims.Provider != "github" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if !hasScope(claims.Scopes, principal.ScopeSignalsRead) || !hasScope(claims.Scopes, principal.ScopeMetadataWrite) {
		t.Fatalf("token missing expected scopes: %+v", claims.Scopes)
	}
	if hasScope(claims.Scopes, principal.ScopeAll) {
		t.Fatalf("job token must not carry ScopeAll")
	}

	release()
	waitState(t, o, spec.JobID, orchestrator.StateSucceeded)

	// The launcher's Handle is recorded on the job for substrate observability.
	if j, ok := o.Job(spec.JobID); !ok || j.Handle.Ref != spec.JobID || j.Handle.Kind != "blocking" {
		t.Fatalf("job handle not recorded: %+v", j.Handle)
	}
}

func TestLauncherErrorMarksJobFailed(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	o := orchestrator.New(m, errLauncher{}, nil)
	defer o.Stop()

	if err := o.EnsureBackfill(context.Background(), "github:2"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		if j, ok := jobForUser(o, "github:2"); ok && j.State == orchestrator.StateFailed {
			if j.Err == "" {
				t.Fatal("failed job should record an error")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("job did not reach failed state")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type errLauncher struct{}

func (errLauncher) Launch(context.Context, orchestrator.JobSpec, string) (orchestrator.Handle, error) {
	return orchestrator.Handle{}, errors.New("boom")
}

// recordingLauncher reports each launched job's type, succeeding immediately.
type recordingLauncher struct {
	seen chan orchestrator.JobType
}

func (r *recordingLauncher) Launch(_ context.Context, spec orchestrator.JobSpec, _ string) (orchestrator.Handle, error) {
	r.seen <- spec.Type
	return orchestrator.Handle{Kind: "recording"}, nil
}

func TestIngestSuccessChainsEnrichment(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	rl := &recordingLauncher{seen: make(chan orchestrator.JobType, 8)}
	o := orchestrator.New(m, rl, nil)
	defer o.Stop()

	if err := o.EnsureBackfill(context.Background(), "github:9"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}

	got := map[orchestrator.JobType]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case ty := <-rl.seen:
			got[ty] = true
		case <-deadline:
			t.Fatalf("expected the full pipeline; saw %v", got)
		}
	}
	if !got[orchestrator.JobGitHubIngest] || !got[orchestrator.JobGitHubEnrich] || !got[orchestrator.JobLLMRank] {
		t.Fatalf("expected ingest, enrich, and llm-rank; saw %v", got)
	}
}

func TestConverseEnqueuesScopedPerItemJob(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	fl := &blockingLauncher{started: make(chan orchestrator.JobSpec, 4), release: make(chan struct{})}
	o := orchestrator.New(m, fl, nil)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fl.release) }) }
	defer o.Stop()
	defer release()

	const user = "github:7"
	const item = "github:octo/repo#1"
	if err := o.Converse(context.Background(), user, item); err != nil {
		t.Fatalf("Converse: %v", err)
	}

	var spec orchestrator.JobSpec
	select {
	case spec = <-fl.started:
	case <-time.After(2 * time.Second):
		t.Fatal("converse runtime did not start")
	}
	if spec.Type != orchestrator.JobGitHubConverse {
		t.Fatalf("job type = %q, want github-converse", spec.Type)
	}
	if spec.ItemID != item || spec.ActingUserID != user || spec.Provider != "github" {
		t.Fatalf("unexpected spec: %+v", spec)
	}

	// A converse for the same user+item while one runs is deduped.
	if err := o.Converse(context.Background(), user, item); err != nil {
		t.Fatalf("second Converse: %v", err)
	}
	select {
	case <-fl.started:
		t.Fatal("expected per-item dedupe, but a second runtime started")
	case <-time.After(150 * time.Millisecond):
	}

	// A different item converses concurrently (the dedupe key includes the item).
	if err := o.Converse(context.Background(), user, "github:octo/repo#2"); err != nil {
		t.Fatalf("Converse other item: %v", err)
	}
	select {
	case <-fl.started:
	case <-time.After(2 * time.Second):
		t.Fatal("a distinct item should converse concurrently")
	}

	// The minted token carries only the converse scopes, never ScopeAll.
	claims, err := m.Verifier().Verify(fl.lastToken())
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if !hasScope(claims.Scopes, principal.ScopeSignalsRead) || !hasScope(claims.Scopes, principal.ScopeMetadataWrite) {
		t.Fatalf("converse token missing scopes: %+v", claims.Scopes)
	}
	if hasScope(claims.Scopes, principal.ScopeAll) {
		t.Fatal("converse token must not carry ScopeAll")
	}

	// Converse jobs must not make the worklist look like it is still ingesting.
	if o.Active(user) {
		t.Fatal("Active should exclude converse jobs")
	}
}

func TestResearchEnqueuesScopedPerItemJobWithoutProvider(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	fl := &blockingLauncher{started: make(chan orchestrator.JobSpec, 4), release: make(chan struct{})}
	o := orchestrator.New(m, fl, nil)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fl.release) }) }
	defer o.Stop()
	defer release()

	const user = "github:7"
	const item = "github:octo/repo#1"
	if err := o.Research(context.Background(), user, item); err != nil {
		t.Fatalf("Research: %v", err)
	}

	var spec orchestrator.JobSpec
	select {
	case spec = <-fl.started:
	case <-time.After(2 * time.Second):
		t.Fatal("research runtime did not start")
	}
	if spec.Type != orchestrator.JobGitHubResearch {
		t.Fatalf("job type = %q, want github-research", spec.Type)
	}
	if spec.ItemID != item || spec.ActingUserID != user {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	// Research reasons over stored ZZ data only — no provider credential.
	if spec.Provider != "" {
		t.Fatalf("research job should carry no provider, got %q", spec.Provider)
	}

	// Per-item dedupe.
	if err := o.Research(context.Background(), user, item); err != nil {
		t.Fatalf("second Research: %v", err)
	}
	select {
	case <-fl.started:
		t.Fatal("expected per-item dedupe, but a second runtime started")
	case <-time.After(150 * time.Millisecond):
	}

	claims, err := m.Verifier().Verify(fl.lastToken())
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if !hasScope(claims.Scopes, principal.ScopeSignalsRead) || !hasScope(claims.Scopes, principal.ScopeMetadataWrite) {
		t.Fatalf("research token missing scopes: %+v", claims.Scopes)
	}
	if claims.Provider != "" {
		t.Fatalf("research token provider = %q, want empty", claims.Provider)
	}

	// Research must not make the worklist look like it is still ingesting.
	if o.Active(user) {
		t.Fatal("Active should exclude research jobs")
	}
}

func hasScope(scopes []principal.Scope, want principal.Scope) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func jobForUser(o *orchestrator.Orchestrator, user string) (orchestrator.Job, bool) {
	for _, j := range o.Jobs() {
		if j.ActingUserID == user {
			return j, true
		}
	}
	return orchestrator.Job{}, false
}

func waitState(t *testing.T, o *orchestrator.Orchestrator, id string, want orchestrator.JobState) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if j, ok := o.Job(id); ok && j.State == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("job %s did not reach state %q", id, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// fakeAsyncLauncher implements orchestrator.AsyncLauncher: Dispatch returns
// immediately (signalling on a channel) and Await blocks until released, so a
// test can hold every job in the awaiting state and prove the dispatch pool is
// not pinned.
type fakeAsyncLauncher struct {
	dispatched chan orchestrator.JobSpec
	release    chan struct{}
}

func (f *fakeAsyncLauncher) Dispatch(_ context.Context, spec orchestrator.JobSpec, _ string) (orchestrator.Handle, error) {
	f.dispatched <- spec
	return orchestrator.Handle{Kind: "fake-async", Ref: spec.JobID}, nil
}

func (f *fakeAsyncLauncher) Await(ctx context.Context, _ orchestrator.Handle) error {
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Launch satisfies orchestrator.Launcher (which AsyncLauncher embeds); the
// orchestrator drives the split Dispatch/Await path, not this.
func (f *fakeAsyncLauncher) Launch(ctx context.Context, spec orchestrator.JobSpec, token string) (orchestrator.Handle, error) {
	h, err := f.Dispatch(ctx, spec, token)
	if err != nil {
		return h, err
	}
	return h, f.Await(ctx, h)
}

func TestAsyncLauncherDoesNotPinDispatchPool(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	fl := &fakeAsyncLauncher{dispatched: make(chan orchestrator.JobSpec, 32), release: make(chan struct{})}
	o := orchestrator.New(m, fl, nil)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fl.release) }) }
	defer o.Stop()
	defer release()

	// More distinct users than the dispatch worker pool. With completion
	// decoupled from dispatch (docs/adr/0024), every job dispatches even while all
	// awaits block; the old blocking model would have pinned the workers and
	// stalled the rest, so the later dispatches would never arrive.
	users := []string{"github:1", "github:2", "github:3", "github:4"}
	for _, u := range users {
		if err := o.EnsureBackfill(context.Background(), u); err != nil {
			t.Fatalf("EnsureBackfill %s: %v", u, err)
		}
	}
	for range users {
		select {
		case <-fl.dispatched:
		case <-time.After(2 * time.Second):
			t.Fatal("not every job dispatched; the dispatch pool was pinned by blocked awaits")
		}
	}

	// Each job is now running (dispatched, awaiting) and already carries the
	// substrate handle Dispatch returned.
	for _, u := range users {
		deadline := time.After(2 * time.Second)
		for {
			j, ok := jobForUser(o, u)
			if ok && j.State == orchestrator.StateRunning && j.Handle.Kind == "fake-async" && j.Handle.Ref != "" {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("job for %s never reached running-with-handle: %+v", u, j)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

func TestMintJobTokenAppliesPolicy(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	o := orchestrator.New(m, orchestrator.NoopLauncher{}, nil)
	defer o.Stop()

	// The pull path issues the same policy-scoped, user-bound token the dispatch
	// path mints (docs/adr/0024), without creating a tracked Job.
	tok, ttl, err := o.MintJobToken(string(orchestrator.JobGitHubIngest), "github:7")
	if err != nil {
		t.Fatalf("MintJobToken: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("expected a positive TTL, got %v", ttl)
	}
	claims, err := m.Verifier().Verify(tok)
	if err != nil {
		t.Fatalf("verify exchanged token: %v", err)
	}
	if claims.ActingUserID != "github:7" || claims.Provider != "github" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if !hasScope(claims.Scopes, principal.ScopeSignalsRead) || !hasScope(claims.Scopes, principal.ScopeMetadataWrite) {
		t.Fatalf("token missing expected scopes: %+v", claims.Scopes)
	}
	if hasScope(claims.Scopes, principal.ScopeAll) {
		t.Fatalf("exchanged token must not carry ScopeAll")
	}

	if _, _, err := o.MintJobToken("nonsense-type", "github:7"); err == nil {
		t.Fatal("expected an error for an unknown job type")
	}
	if _, _, err := o.MintJobToken(string(orchestrator.JobGitHubIngest), ""); err == nil {
		t.Fatal("expected an error for an empty acting user")
	}
}

func TestCallbackCompletesBeforeWatch(t *testing.T) {
	m := mint.NewMinterFromSeed([]byte(testSecret), time.Minute)
	// The launcher's Await blocks forever (release is never closed), so the only
	// way the job can finish is the runtime's completion callback (docs/adr/0024).
	fl := &fakeAsyncLauncher{dispatched: make(chan orchestrator.JobSpec, 4), release: make(chan struct{})}
	o := orchestrator.New(m, fl, nil)
	defer o.Stop()

	const user, item = "github:7", "github:octo/repo#1"
	// Research is per-item and does not chain, so the job stands alone.
	if err := o.Research(context.Background(), user, item); err != nil {
		t.Fatalf("Research: %v", err)
	}
	select {
	case <-fl.dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("research job never dispatched")
	}

	// Signal completion via the callback; the launcher's watch stays blocked.
	// Retried because registration races the dispatch worker — an unregistered
	// jobID is a no-op, so the loop converges once the await goroutine arms it.
	deadline := time.After(2 * time.Second)
	for {
		if j, ok := jobForUser(o, user); ok {
			o.CompleteJob(j.ID, "", runtimestats.Timing{})
			if jj, _ := o.Job(j.ID); jj.State == orchestrator.StateSucceeded {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("job never finished from the completion callback (the watch was never released)")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
