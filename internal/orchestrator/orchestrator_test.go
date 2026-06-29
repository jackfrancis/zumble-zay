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

func TestEnsureBackfillDedupesAndMintsScopedToken(t *testing.T) {
	m := mint.NewMinter([]byte(testSecret), time.Minute)
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
	claims, err := m.Verify(fl.lastToken())
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
	m := mint.NewMinter([]byte(testSecret), time.Minute)
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
	m := mint.NewMinter([]byte(testSecret), time.Minute)
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
	m := mint.NewMinter([]byte(testSecret), time.Minute)
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
	claims, err := m.Verify(fl.lastToken())
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
	m := mint.NewMinter([]byte(testSecret), time.Minute)
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

	claims, err := m.Verify(fl.lastToken())
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
