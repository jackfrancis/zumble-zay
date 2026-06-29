// Package reconcile periodically re-ranks work items whose discussion-derived
// research has gone stale, by enqueuing per-item research jobs (docs/adr/0022).
// It is the trigger for the research agent (slice 3): not per discussion entry,
// but an aggressive timer that catches up within a few minutes. It reads the
// worklist through worklist.Lister and enqueues through the ResearchEnqueuer
// seam, so it couples to neither the store backend nor the control plane.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// ResearchEnqueuer schedules a per-item research re-rank. The orchestrator
// satisfies it via Research; depending only on this seam keeps the reconciler
// from importing the control plane (docs/adr/0022), mirroring worklist.Ingestor.
type ResearchEnqueuer interface {
	Research(ctx context.Context, ownerID, itemID string) error
}

// DefaultInterval is the reconcile cadence. Re-ranking after every discussion
// entry is overkill; an aggressive timer catches up within a few minutes.
const DefaultInterval = 3 * time.Minute

// Reconciler periodically scans stored items and enqueues research re-ranks for
// those whose research is stale relative to their thread or GitHub freshness. At
// replicas:1 it is a single goroutine; past that it must be leader-gated, since a
// reconciler wants a single leader (docs/adr/0007).
type Reconciler struct {
	lister   worklist.Lister
	enqueuer ResearchEnqueuer
	interval time.Duration
	log      *slog.Logger
	stop     chan struct{}
	done     chan struct{}
}

// New builds a Reconciler. A non-positive interval uses DefaultInterval.
func New(lister worklist.Lister, enqueuer ResearchEnqueuer, interval time.Duration, log *slog.Logger) *Reconciler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Reconciler{
		lister:   lister,
		enqueuer: enqueuer,
		interval: interval,
		log:      log,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the reconcile loop. Call Stop to end it.
func (r *Reconciler) Start() { go r.loop() }

func (r *Reconciler) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.ReconcileOnce(context.Background())
		}
	}
}

// Stop ends the loop and waits for it to return.
func (r *Reconciler) Stop() {
	close(r.stop)
	<-r.done
}

// ReconcileOnce scans every owner's items once and enqueues a research re-rank
// for each stale item. Enqueue is idempotent — the orchestrator dedups per item
// — so a burst of discussion collapses to one job per item. Exported for tests.
func (r *Reconciler) ReconcileOnce(ctx context.Context) {
	all, err := r.lister.All(ctx)
	if err != nil {
		if r.log != nil {
			r.log.Warn("reconcile: list items", "err", err)
		}
		return
	}
	var enqueued int
	for owner, items := range all {
		for i := range items {
			if !researchStale(items[i]) {
				continue
			}
			if err := r.enqueuer.Research(ctx, owner, items[i].ID); err != nil {
				if r.log != nil {
					r.log.Warn("reconcile: enqueue research", "owner", owner, "item", items[i].ID, "err", err)
				}
				continue
			}
			enqueued++
		}
	}
	if enqueued > 0 && r.log != nil {
		r.log.Info("reconcile: enqueued research re-ranks", "count", enqueued)
	}
}

// researchStale reports whether an item's research re-weighting is stale relative
// to its conversation thread or its GitHub freshness (docs/adr/0022). Only items
// with a thread are eligible; one that has never been researched is stale, and
// one whose research predates the newest input (a later message, or a GitHub
// update that likely moved the foundation) is stale. It keys off the research's
// own AppliedAt rather than Meta.ScoredAt, which either pass bumps — so a fresh
// foundation rank does not falsely look like fresh research.
func researchStale(it worklist.WorkItem) bool {
	if len(it.Thread) == 0 {
		return false
	}
	freshest := it.GitHub.UpdatedAt
	if last := lastMessageAt(it.Thread); last.After(freshest) {
		freshest = last
	}
	rsh := it.Signals.Research
	if rsh == nil {
		return true
	}
	return rsh.AppliedAt.Before(freshest)
}

func lastMessageAt(thread []worklist.Message) time.Time {
	var latest time.Time
	for _, m := range thread {
		if m.At.After(latest) {
			latest = m.At
		}
	}
	return latest
}
