// Package orchestrator is ZZ's co-located control plane for agent runtimes (see
// docs/adr/0002 and docs/adr/0007). It accepts ingestion requests, tracks each
// as a job through a lifecycle, computes the job's authorization, mints a
// job-scoped token, and dispatches an ephemeral runtime through a Launcher.
//
// It deliberately knows nothing about any provider — only agent runtimes talk
// to GitHub/Graph (docs/adr/0006). The Launcher seam lets the in-process
// runtime be swapped for spawned Kubernetes Pods without changing this code.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/mint"
	"github.com/jackfrancis/zumble-zay/internal/principal"
)

// JobType identifies a kind of agent work and selects its runtime-type policy.
type JobType string

// JobGitHubIngest retrieves a user's GitHub work items.
const JobGitHubIngest JobType = "github-ingest"

// JobGitHubEnrich refines a user's existing GitHub work items with expensive,
// per-item signals (e.g. AwaitingMeSince) that need extra API calls. It is a
// distinct capability from ingestion — its own scopes, rate-limit budget, and
// failure domain — so it can be scaled or throttled independently and later run
// as its own out-of-process runtime (docs/adr/0009).
const JobGitHubEnrich JobType = "github-enrich"

// JobLLMRank produces the four ranking axes for a user's items via an LLM and
// writes them back as a proposal ZZ ratifies against the deterministic baseline
// (docs/adr/0011). It reads and writes ZZ only — no provider credential — so its
// policy grants no provider.
const JobLLMRank JobType = "llm-rank"

// JobState is a point in a job's lifecycle.
type JobState string

const (
	StatePending   JobState = "pending"
	StateRunning   JobState = "running"
	StateSucceeded JobState = "succeeded"
	StateFailed    JobState = "failed"
)

// JobSpec is what a runtime needs to execute. The job token (minted separately)
// carries the acting user and granted scopes; the spec carries the task shape.
type JobSpec struct {
	JobID        string
	Type         JobType
	Provider     string
	ActingUserID string
}

// Handle identifies a launched workload and where it ran, so the orchestrator
// can observe and (later) reconcile it against the substrate (docs/adr/0012).
// For a synchronous launcher the terminal outcome is the Launch error; the
// Handle adds the workload's identity/location.
type Handle struct {
	Kind string // launcher kind, e.g. "inprocess", "k8s-job"
	Ref  string // substrate-specific id (Pod/Job name); empty in-process
}

// Launcher executes a runtime for a job, returning a Handle describing where it
// ran. The in-process launcher runs the agent inline and returns on completion;
// a future Kubernetes launcher creates a Pod/Job and watches it to completion
// behind this same interface (docs/adr/0009, 0012).
type Launcher interface {
	Launch(ctx context.Context, spec JobSpec, token string) (Handle, error)
}

// NoopLauncher succeeds without doing anything. It is the default when no
// runtime is wired, keeping EnsureBackfill safe in tests and minimal setups.
type NoopLauncher struct{}

// Launch does nothing and succeeds.
func (NoopLauncher) Launch(context.Context, JobSpec, string) (Handle, error) {
	return Handle{Kind: "noop"}, nil
}

// Job is the tracked lifecycle record for one unit of agent work.
type Job struct {
	ID           string
	Type         JobType
	Provider     string
	ActingUserID string
	State        JobState
	Handle       Handle // where the workload ran (substrate observability)
	Err          string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// policyEntry is the runtime-type policy: the provider and scopes a job type may
// request. The minted scope set is this policy intersected with the user's
// consent (today, login is taken as consent to the provider; when consent
// becomes explicit and granular it intersects here).
type policyEntry struct {
	provider string
	scopes   []principal.Scope
}

var policies = map[JobType]policyEntry{
	JobGitHubIngest: {
		provider: "github",
		scopes:   []principal.Scope{principal.ScopeSignalsRead, principal.ScopeMetadataWrite},
	},
	JobGitHubEnrich: {
		provider: "github",
		scopes:   []principal.Scope{principal.ScopeSignalsRead, principal.ScopeMetadataWrite},
	},
	JobLLMRank: {
		provider: "",
		scopes:   []principal.Scope{principal.ScopeSignalsRead, principal.ScopeMetadataWrite},
	},
}

const (
	defaultWorkers = 2
	// Fan-out jobs (e.g. github-enrich) make many provider calls, so the per-job
	// deadline must leave generous headroom; the in-process runtimes are bounded
	// anyway. Too tight a deadline cancels the job mid-flight, discards its work,
	// and breaks the pipeline chain.
	defaultJobTTL = 2 * time.Minute
	// defaultRankJobTTL is the llm-rank budget. That stage makes one slow
	// chat-model call per shortlisted item (seconds each, more with adaptive
	// thinking), so it needs more headroom than the bounded GitHub fan-outs; a
	// full pass otherwise approaches the general deadline.
	defaultRankJobTTL = 5 * time.Minute
	queueDepth        = 128
)

// Orchestrator accepts ingestion requests and supervises agent runtimes.
type Orchestrator struct {
	minter   *mint.Minter
	launcher Launcher
	log      *slog.Logger
	jobTTL   time.Duration

	queue chan string // jobID

	mu       sync.Mutex
	jobs     map[string]*Job
	inflight map[string]bool // dedupe key: actingUserID + "/" + JobType
	closed   bool

	wg sync.WaitGroup
}

// New starts an orchestrator with a small worker pool. A nil launcher uses
// NoopLauncher. Call Stop to drain workers.
func New(minter *mint.Minter, launcher Launcher, log *slog.Logger) *Orchestrator {
	if launcher == nil {
		launcher = NoopLauncher{}
	}
	o := &Orchestrator{
		minter:   minter,
		launcher: launcher,
		log:      log,
		jobTTL:   defaultJobTTL,
		queue:    make(chan string, queueDepth),
		jobs:     make(map[string]*Job),
		inflight: make(map[string]bool),
	}
	for i := 0; i < defaultWorkers; i++ {
		o.wg.Add(1)
		go o.reconcile()
	}
	return o
}

// EnsureBackfill implements worklist.Ingestor: it enqueues a github-ingest job
// for ownerID unless one is already in flight, and returns immediately so it is
// safe to call from the request path.
func (o *Orchestrator) EnsureBackfill(_ context.Context, ownerID string) error {
	if ownerID == "" {
		return fmt.Errorf("orchestrator: empty ownerID")
	}
	return o.enqueue(JobGitHubIngest, ownerID)
}

func (o *Orchestrator) enqueue(t JobType, ownerID string) error {
	pol, ok := policies[t]
	if !ok {
		return fmt.Errorf("orchestrator: unknown job type %q", t)
	}
	key := ownerID + "/" + string(t)

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("orchestrator: closed")
	}
	if o.inflight[key] {
		o.mu.Unlock()
		return nil // idempotent: a job for this user+type is already pending/running
	}
	id := newID()
	now := time.Now().UTC()
	o.jobs[id] = &Job{
		ID: id, Type: t, Provider: pol.provider, ActingUserID: ownerID,
		State: StatePending, CreatedAt: now, UpdatedAt: now,
	}
	o.inflight[key] = true
	o.mu.Unlock()

	select {
	case o.queue <- id:
		return nil
	default:
		o.mu.Lock()
		o.jobs[id].State = StateFailed
		o.jobs[id].Err = "queue full"
		o.jobs[id].UpdatedAt = time.Now().UTC()
		delete(o.inflight, key)
		o.mu.Unlock()
		return fmt.Errorf("orchestrator: queue full")
	}
}

func (o *Orchestrator) reconcile() {
	defer o.wg.Done()
	for id := range o.queue {
		o.run(id)
	}
}

func (o *Orchestrator) run(id string) {
	o.mu.Lock()
	job := o.jobs[id]
	if job == nil {
		o.mu.Unlock()
		return
	}
	job.State = StateRunning
	job.UpdatedAt = time.Now().UTC()
	pol := policies[job.Type]
	spec := JobSpec{JobID: job.ID, Type: job.Type, Provider: job.Provider, ActingUserID: job.ActingUserID}
	key := job.ActingUserID + "/" + string(job.Type)
	o.mu.Unlock()

	// Authorization minted at spawn: runtime-type policy ∩ user consent. The
	// token's subject is the ephemeral runtime, so writes trace runtime → job →
	// user (docs/adr/0002).
	token, err := o.minter.Mint(mint.Claims{
		Subject:      "runtime-" + job.ID,
		ActingUserID: job.ActingUserID,
		Scopes:       pol.scopes,
		JobID:        job.ID,
		Provider:     pol.provider,
	})
	var handle Handle
	if err == nil {
		handle, err = o.safeLaunch(spec, token)
	}

	o.mu.Lock()
	job.Handle = handle
	if err != nil {
		job.State = StateFailed
		job.Err = err.Error()
		if o.log != nil {
			o.log.Error("agent job failed", "job", job.ID, "user", job.ActingUserID, "type", job.Type, "err", err)
		}
	} else {
		job.State = StateSucceeded
		if o.log != nil {
			o.log.Info("agent job succeeded", "job", job.ID, "user", job.ActingUserID, "type", job.Type)
		}
	}
	job.UpdatedAt = time.Now().UTC()
	jobType, user := job.Type, job.ActingUserID
	delete(o.inflight, key)
	o.mu.Unlock()

	// Pipeline: each successful stage hands off to the next (ingest -> enrich ->
	// llm-rank). Each stage is a distinct capability (its own scopes, rate-limit
	// budget, and failure domain); the final stage does not chain further.
	if err == nil {
		if next, ok := nextStage(jobType); ok {
			if e := o.enqueue(next, user); e != nil && o.log != nil {
				o.log.Warn("could not enqueue next pipeline stage", "stage", next, "user", user, "err", e)
			}
		}
	}
}

// safeLaunch invokes the launcher with the per-job deadline, converting a
// launcher panic into an ordinary job failure. A launcher runs substrate code
// (a client library, a nil dereference) outside the request goroutine, so
// without this a panic would crash the whole server rather than failing one
// job — recoverer only guards request goroutines.
func (o *Orchestrator) safeLaunch(spec JobSpec, token string) (handle Handle, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("launcher panicked: %v", rec)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), o.deadlineFor(spec.Type))
	defer cancel()
	return o.launcher.Launch(ctx, spec, token)
}

// deadlineFor returns the execution budget for a job type. The llm-rank stage
// calls a slow chat model once per shortlisted item, so it gets more headroom
// than the bounded GitHub API fan-outs. A larger configured jobTTL still wins.
func (o *Orchestrator) deadlineFor(t JobType) time.Duration {
	if t == JobLLMRank && o.jobTTL < defaultRankJobTTL {
		return defaultRankJobTTL
	}
	return o.jobTTL
}

// nextStage returns the capability that follows t in the ingestion pipeline.
func nextStage(t JobType) (JobType, bool) {
	switch t {
	case JobGitHubIngest:
		return JobGitHubEnrich, true
	case JobGitHubEnrich:
		return JobLLMRank, true
	default:
		return "", false
	}
}

// Job returns a copy of the tracked job, if present. Intended for status and
// tests.
func (o *Orchestrator) Job(id string) (Job, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	j, ok := o.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Jobs returns a snapshot of all tracked jobs, for status and tests.
func (o *Orchestrator) Jobs() []Job {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Job, 0, len(o.jobs))
	for _, j := range o.jobs {
		out = append(out, *j)
	}
	return out
}

// Active reports whether any job for ownerID is still pending or running, i.e.
// an ingest/enrich/llm-rank pass is in flight for that user. The UI uses it to
// keep auto-refreshing the worklist until ranking settles (docs/adr/0016).
func (o *Orchestrator) Active(ownerID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, j := range o.jobs {
		if j.ActingUserID == ownerID && (j.State == StatePending || j.State == StateRunning) {
			return true
		}
	}
	return false
}

// Stop drains the worker pool. After Stop, EnsureBackfill returns an error.
func (o *Orchestrator) Stop() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	o.mu.Unlock()
	close(o.queue)
	o.wg.Wait()
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("orchestrator: failed to read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}
