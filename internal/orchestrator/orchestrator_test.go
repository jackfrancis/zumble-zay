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

func (b *blockingLauncher) Launch(_ context.Context, spec orchestrator.JobSpec, token string) error {
	b.mu.Lock()
	b.tokens = append(b.tokens, token)
	b.mu.Unlock()
	b.started <- spec
	<-b.release
	return nil
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

func (errLauncher) Launch(context.Context, orchestrator.JobSpec, string) error {
	return errors.New("boom")
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
