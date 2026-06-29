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

// JobGitHubConverse answers one turn of a user's assistive conversation about a
// single item (docs/adr/0019). Unlike the pipeline stages it is per-item — its
// spec carries an ItemID — and it both reads GitHub (the live PR description,
// discussion, and changed files, for context) and writes ZZ (the agent's reply,
// appended to the item's thread). It does not chain to any further stage.
const JobGitHubConverse JobType = "github-converse"

// JobGitHubResearch re-weights a single item's ranking axes from its
// conversation thread (docs/adr/0022). It is per-item (its spec carries an
// ItemID) and reads and writes ZZ only — the cached foundation plus the thread
// in, the research multipliers out — so it needs no provider credential. It does
// not chain to any further stage.
const JobGitHubResearch JobType = "github-research"

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
	// ItemID scopes a per-item job (e.g. github-converse) to one work item; it is
	// empty for the whole-worklist pipeline stages.
	ItemID string
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

// AsyncLauncher is an optional capability a Launcher may add to separate
// dispatching a workload from awaiting its completion (docs/adr/0024). The
// orchestrator prefers it when present: Dispatch runs on the bounded worker pool
// (so concurrent substrate creates stay bounded), while Await runs on its own
// goroutine (so a long-running job never pins a worker, and completion can be
// observed by watching the substrate). Await must derive everything it needs
// from the Handle, so the orchestrator can call it on a separate goroutine with
// no shared launcher state — and the orchestrator, owning that goroutine, keeps
// the same panic isolation and per-job deadline it applies to a blocking Launch.
// A Launcher that does not implement this is still fully supported: its blocking
// Launch is simply run off the dispatch worker.
type AsyncLauncher interface {
	Launcher
	Dispatch(ctx context.Context, spec JobSpec, token string) (Handle, error)
	Await(ctx context.Context, handle Handle) error
}

// Job is the tracked lifecycle record for one unit of agent work.
type Job struct {
	ID           string
	Type         JobType
	Provider     string
	ActingUserID string
	ItemID       string // set for per-item jobs (github-converse); empty otherwise
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
	JobGitHubConverse: {
		provider: "github",
		scopes:   []principal.Scope{principal.ScopeSignalsRead, principal.ScopeMetadataWrite},
	},
	JobGitHubResearch: {
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
	// full pass otherwise approaches the general deadline. The per-item converse
	// job shares this budget: it makes a live GitHub fetch plus one slow
	// chat-model call (docs/adr/0019).
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

	mu          sync.Mutex
	jobs        map[string]*Job
	inflight    map[string]bool       // dedupe key: actingUserID + "/" + JobType
	completions map[string]chan error // jobID -> runtime callback completion signal (docs/adr/0024)
	closed      bool

	wg       sync.WaitGroup // dispatch workers draining the queue
	awaiters sync.WaitGroup // in-flight launch/await goroutines (docs/adr/0024)
}

// New starts an orchestrator with a small worker pool. A nil launcher uses
// NoopLauncher. Call Stop to drain workers.
func New(minter *mint.Minter, launcher Launcher, log *slog.Logger) *Orchestrator {
	if launcher == nil {
		launcher = NoopLauncher{}
	}
	o := &Orchestrator{
		minter:      minter,
		launcher:    launcher,
		log:         log,
		jobTTL:      defaultJobTTL,
		queue:       make(chan string, queueDepth),
		jobs:        make(map[string]*Job),
		inflight:    make(map[string]bool),
		completions: make(map[string]chan error),
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
	return o.enqueue(JobGitHubIngest, ownerID, "")
}

// Converse enqueues a github-converse job that answers one turn of the assistive
// conversation about a single item (docs/adr/0019). It returns immediately so it
// is safe to call from the request path; the spawned runtime fetches live GitHub
// context, asks the model, and writes the reply back to the item's thread. A
// converse already in flight for the same user+item is deduped.
func (o *Orchestrator) Converse(_ context.Context, ownerID, itemID string) error {
	if ownerID == "" || itemID == "" {
		return fmt.Errorf("orchestrator: converse requires ownerID and itemID")
	}
	return o.enqueue(JobGitHubConverse, ownerID, itemID)
}

// Research enqueues a github-research job that re-weights one item's ranking
// axes from its conversation thread (docs/adr/0022). It returns immediately so it
// is safe to call from a reconcile loop; the spawned runtime reads the item and
// its thread, asks the model for the per-axis multipliers, and writes them back.
// A research already in flight for the same user+item is deduped.
func (o *Orchestrator) Research(_ context.Context, ownerID, itemID string) error {
	if ownerID == "" || itemID == "" {
		return fmt.Errorf("orchestrator: research requires ownerID and itemID")
	}
	return o.enqueue(JobGitHubResearch, ownerID, itemID)
}

// MintJobToken issues a job-scoped token for an authenticated control-plane
// caller (docs/adr/0024): the pull complement to the push-at-dispatch path, for
// long-lived service runtimes (e.g. kagent) that are not born per job and so
// request a fresh token per task rather than receiving one at spawn. It applies
// the same runtime-type policy as dispatch (the job type's provider and scopes)
// and returns the signed token with its lifetime. It does not create a tracked
// Job — the caller runs the work itself; provenance still flows from the token's
// job id. Caller authentication and any per-caller constraint are enforced before
// this is reached (the control API's token endpoint).
func (o *Orchestrator) MintJobToken(jobType, actingUser string) (string, time.Duration, error) {
	if actingUser == "" {
		return "", 0, fmt.Errorf("orchestrator: token request requires an acting user")
	}
	pol, ok := policies[JobType(jobType)]
	if !ok {
		return "", 0, fmt.Errorf("orchestrator: unknown job type %q", jobType)
	}
	jid := "exch-" + newID()
	tok, err := o.minter.Mint(mint.Claims{
		Subject:      "runtime-" + jid,
		ActingUserID: actingUser,
		Scopes:       pol.scopes,
		JobID:        jid,
		Provider:     pol.provider,
	})
	if err != nil {
		return "", 0, err
	}
	return tok, o.minter.TTL(), nil
}

// enqueue records a job and queues it for a worker. itemID is empty for the
// whole-worklist pipeline stages and set for per-item jobs (github-converse); it
// is part of the dedupe key so distinct items can converse concurrently while a
// repeated turn for the same item is a no-op.
func (o *Orchestrator) enqueue(t JobType, ownerID, itemID string) error {
	pol, ok := policies[t]
	if !ok {
		return fmt.Errorf("orchestrator: unknown job type %q", t)
	}
	key := ownerID + "/" + string(t)
	if itemID != "" {
		key += "/" + itemID
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return fmt.Errorf("orchestrator: closed")
	}
	if o.inflight[key] {
		return nil // idempotent: a job for this dedupe key is already pending/running
	}
	id := newID()
	now := time.Now().UTC()
	o.jobs[id] = &Job{
		ID: id, Type: t, Provider: pol.provider, ActingUserID: ownerID, ItemID: itemID,
		State: StatePending, CreatedAt: now, UpdatedAt: now,
	}
	// Enqueue under the lock with a non-blocking send, paired with Stop closing
	// the queue under the same lock: a send therefore never races the close (no
	// send-on-closed-channel panic), and the non-blocking select cannot deadlock
	// while the lock is held (workers receive without it).
	select {
	case o.queue <- id:
		o.inflight[key] = true
		return nil
	default:
		o.jobs[id].State = StateFailed
		o.jobs[id].Err = "queue full"
		o.jobs[id].UpdatedAt = time.Now().UTC()
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
	spec := JobSpec{JobID: job.ID, Type: job.Type, Provider: job.Provider, ActingUserID: job.ActingUserID, ItemID: job.ItemID}
	key := job.ActingUserID + "/" + string(job.Type)
	if job.ItemID != "" {
		key += "/" + job.ItemID
	}
	o.mu.Unlock()

	// Authorization minted at spawn: runtime-type policy ∩ user consent. The
	// token's subject is the ephemeral runtime, so writes trace runtime → job →
	// user (docs/adr/0002).
	token, err := o.minter.Mint(mint.Claims{
		Subject:      "runtime-" + spec.JobID,
		ActingUserID: spec.ActingUserID,
		Scopes:       pol.scopes,
		JobID:        spec.JobID,
		Provider:     pol.provider,
	})
	if err != nil {
		o.finish(id, key, Handle{}, err)
		return
	}

	// Completion is decoupled from the dispatch worker (docs/adr/0024): the await
	// runs on its own goroutine so a slow job never pins a worker, and the
	// per-job deadline bounds it. An AsyncLauncher splits create (Dispatch, on the
	// worker so concurrent creates stay bounded) from watch (Await, off it); a
	// plain Launcher's blocking Launch is run off the worker just the same.
	ctx, cancel := context.WithTimeout(context.Background(), o.deadlineFor(spec.Type))

	if al, ok := o.launcher.(AsyncLauncher); ok {
		handle, derr := o.safeDispatch(al, ctx, spec, token)
		if derr != nil {
			cancel()
			o.finish(id, key, handle, derr)
			return
		}
		o.setHandle(id, handle)
		completion := o.registerCompletion(spec.JobID)
		o.awaiters.Add(1)
		go func() {
			defer o.awaiters.Done()
			defer cancel()
			defer o.deregisterCompletion(spec.JobID)
			// Completion can arrive two ways (docs/adr/0009, 0024): the runtime's
			// callback (the fast happy path) or the launcher's substrate watch (the
			// failure/timeout backstop). Race them; the first wins and the deferred
			// ctx cancel unwinds the loser.
			watch := make(chan error, 1)
			go func() { watch <- o.safeAwait(al, ctx, handle) }()
			select {
			case err := <-completion:
				o.finish(id, key, handle, err)
			case err := <-watch:
				o.finish(id, key, handle, err)
			}
		}()
		return
	}

	o.awaiters.Add(1)
	go func() {
		defer o.awaiters.Done()
		defer cancel()
		handle, lerr := o.safeLaunch(ctx, spec, token)
		o.finish(id, key, handle, lerr)
	}()
}

// finish records a job's terminal outcome, drops it from the in-flight set, and
// chains the next pipeline stage on success. It is the single place a job is
// finalized — called exactly once per job, from whichever goroutine awaited it.
func (o *Orchestrator) finish(id, key string, handle Handle, err error) {
	o.mu.Lock()
	job := o.jobs[id]
	if job == nil {
		o.mu.Unlock()
		return
	}
	job.Handle = handle
	if err != nil {
		job.State = StateFailed
		job.Err = err.Error()
	} else {
		job.State = StateSucceeded
	}
	job.UpdatedAt = time.Now().UTC()
	jobType, user := job.Type, job.ActingUserID
	delete(o.inflight, key)
	o.mu.Unlock()

	if err != nil {
		if o.log != nil {
			o.log.Error("agent job failed", "job", id, "user", user, "type", jobType, "err", err)
		}
		return
	}
	if o.log != nil {
		o.log.Info("agent job succeeded", "job", id, "user", user, "type", jobType)
	}

	// Pipeline: each successful stage hands off to the next (ingest -> enrich ->
	// llm-rank). Each stage is a distinct capability (its own scopes, rate-limit
	// budget, and failure domain); the final stage does not chain further.
	if next, ok := nextStage(jobType); ok {
		if e := o.enqueue(next, user, ""); e != nil && o.log != nil {
			o.log.Warn("could not enqueue next pipeline stage", "stage", next, "user", user, "err", e)
		}
	}
}

// setHandle records where an async job's workload was dispatched, before it
// completes, so a running job already shows its substrate location (docs/adr/0012).
func (o *Orchestrator) setHandle(id string, handle Handle) {
	o.mu.Lock()
	if job := o.jobs[id]; job != nil {
		job.Handle = handle
	}
	o.mu.Unlock()
}

// registerCompletion arms a one-shot channel the runtime's completion callback
// can signal for jobID (docs/adr/0024); the await goroutine races it against the
// launcher's watch. deregisterCompletion clears it once the job is finalized.
func (o *Orchestrator) registerCompletion(jobID string) chan error {
	ch := make(chan error, 1)
	o.mu.Lock()
	o.completions[jobID] = ch
	o.mu.Unlock()
	return ch
}

func (o *Orchestrator) deregisterCompletion(jobID string) {
	o.mu.Lock()
	delete(o.completions, jobID)
	o.mu.Unlock()
}

// CompleteJob delivers a runtime's terminal completion for jobID — an empty
// errMsg is success, otherwise failure (docs/adr/0024). It is the pull-side
// counterpart to the launcher's watch: whichever reports first finalizes the
// job. An unknown or already-finalized jobID is a no-op (the watch handled it,
// or there is nothing to await — e.g. an in-process job whose completion is its
// Launch return).
func (o *Orchestrator) CompleteJob(jobID, errMsg string) {
	o.mu.Lock()
	ch, ok := o.completions[jobID]
	o.mu.Unlock()
	if !ok {
		return
	}
	var err error
	if errMsg != "" {
		err = fmt.Errorf("runtime reported failure: %s", errMsg)
	}
	select {
	case ch <- err:
	default: // already signalled; ignore the duplicate
	}
}

// safeLaunch invokes a blocking launcher under ctx, converting a launcher panic
// into an ordinary job failure. A launcher runs substrate code (a client
// library, a nil dereference) outside the request goroutine, so without this a
// panic would crash the whole process rather than failing one job — recoverer
// only guards request goroutines.
func (o *Orchestrator) safeLaunch(ctx context.Context, spec JobSpec, token string) (handle Handle, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("launcher panicked: %v", rec)
		}
	}()
	return o.launcher.Launch(ctx, spec, token)
}

// safeDispatch starts an async launcher's workload, recovering a panic as a job
// failure (as safeLaunch does for blocking launchers).
func (o *Orchestrator) safeDispatch(al AsyncLauncher, ctx context.Context, spec JobSpec, token string) (handle Handle, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("launcher panicked: %v", rec)
		}
	}()
	return al.Dispatch(ctx, spec, token)
}

// safeAwait waits for an async launcher's workload to complete, recovering a
// panic as a job failure. The orchestrator owns this goroutine (not the
// launcher), so the same panic guard and per-job deadline apply as for a
// blocking Launch.
func (o *Orchestrator) safeAwait(al AsyncLauncher, ctx context.Context, handle Handle) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("launcher panicked: %v", rec)
		}
	}()
	return al.Await(ctx, handle)
}

// deadlineFor returns the execution budget for a job type. The llm-rank and
// per-item converse stages call a slow chat model, so they get more headroom
// than the bounded GitHub API fan-outs. A larger configured jobTTL still wins.
func (o *Orchestrator) deadlineFor(t JobType) time.Duration {
	if (t == JobLLMRank || t == JobGitHubConverse || t == JobGitHubResearch) && o.jobTTL < defaultRankJobTTL {
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

// Active reports whether any pipeline job for ownerID is still pending or
// running, i.e. an ingest/enrich/llm-rank pass is in flight for that user. The
// UI uses it to keep auto-refreshing the worklist until ranking settles
// (docs/adr/0016). Per-item converse jobs are excluded: they are interactive and
// must not make the radar look like it is still ingesting (docs/adr/0019).
func (o *Orchestrator) Active(ownerID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, j := range o.jobs {
		if j.ActingUserID != ownerID || j.Type == JobGitHubConverse || j.Type == JobGitHubResearch {
			continue
		}
		if j.State == StatePending || j.State == StateRunning {
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
	// Close the queue under the lock, paired with enqueue's send under the lock,
	// so a chaining send can never race this close.
	close(o.queue)
	o.mu.Unlock()
	o.wg.Wait()       // dispatch workers drain
	o.awaiters.Wait() // in-flight launches/awaits finish (bounded by the per-job deadline)
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("orchestrator: failed to read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}
