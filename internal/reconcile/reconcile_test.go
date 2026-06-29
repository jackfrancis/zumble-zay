package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

func msgAt(at time.Time) worklist.Message {
	return worklist.Message{Role: worklist.RoleUser, Content: "x", At: at}
}

func TestResearchStale(t *testing.T) {
	now := time.Now().UTC()
	older := now.Add(-time.Hour)
	newer := now.Add(time.Hour)

	cases := []struct {
		name string
		it   worklist.WorkItem
		want bool
	}{
		{"no thread is never stale", worklist.WorkItem{}, false},
		{"thread but never researched is stale", worklist.WorkItem{
			Thread: []worklist.Message{msgAt(now)},
		}, true},
		{"research older than the last message is stale", worklist.WorkItem{
			Thread:  []worklist.Message{msgAt(newer)},
			Signals: worklist.Signals{Research: &worklist.ResearchAdjustment{AppliedAt: now}},
		}, true},
		{"research newer than all inputs is fresh", worklist.WorkItem{
			Thread:  []worklist.Message{msgAt(older)},
			GitHub:  worklist.GitHubRef{UpdatedAt: older},
			Signals: worklist.Signals{Research: &worklist.ResearchAdjustment{AppliedAt: now}},
		}, false},
		{"research older than a github update is stale", worklist.WorkItem{
			Thread:  []worklist.Message{msgAt(older)},
			GitHub:  worklist.GitHubRef{UpdatedAt: newer},
			Signals: worklist.Signals{Research: &worklist.ResearchAdjustment{AppliedAt: now}},
		}, true},
	}
	for _, c := range cases {
		if got := researchStale(c.it); got != c.want {
			t.Errorf("%s: researchStale = %v, want %v", c.name, got, c.want)
		}
	}
}

type fakeLister struct {
	items map[string][]worklist.WorkItem
}

func (f fakeLister) All(context.Context) (map[string][]worklist.WorkItem, error) {
	return f.items, nil
}

type recordingEnqueuer struct{ calls []string }

func (r *recordingEnqueuer) Research(_ context.Context, owner, item string) error {
	r.calls = append(r.calls, owner+"/"+item)
	return nil
}

func TestReconcileOnceEnqueuesOnlyStaleItems(t *testing.T) {
	now := time.Now().UTC()
	lister := fakeLister{items: map[string][]worklist.WorkItem{
		"u1": {
			// thread, no research yet -> stale.
			{ID: "stale", Thread: []worklist.Message{msgAt(now)}},
			// researched after its inputs -> fresh.
			{ID: "fresh",
				Thread:  []worklist.Message{msgAt(now.Add(-time.Hour))},
				Signals: worklist.Signals{Research: &worklist.ResearchAdjustment{AppliedAt: now}}},
			// no thread -> skipped.
			{ID: "nothread"},
		},
	}}
	enq := &recordingEnqueuer{}
	r := New(lister, enq, time.Minute, nil)

	r.ReconcileOnce(context.Background())

	if len(enq.calls) != 1 || enq.calls[0] != "u1/stale" {
		t.Fatalf("expected only u1/stale enqueued, got %v", enq.calls)
	}
}
