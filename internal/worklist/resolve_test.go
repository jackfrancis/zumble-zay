package worklist

import (
	"context"
	"testing"
	"time"
)

type noopIngestor struct{ called bool }

func (n *noopIngestor) EnsureBackfill(context.Context, string) error {
	n.called = true
	return nil
}

func TestHiddenAfter(t *testing.T) {
	hid := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	if got := HiddenAfter(time.Time{}, hid); !got.IsZero() {
		t.Errorf("never hidden -> zero, got %v", got)
	}
	if got := HiddenAfter(hid, hid.Add(-time.Hour)); !got.Equal(hid) {
		t.Errorf("unchanged since hidden -> stay hidden, got %v", got)
	}
	if got := HiddenAfter(hid, hid.Add(time.Hour)); !got.IsZero() {
		t.Errorf("updated after hidden -> auto-unhide (zero), got %v", got)
	}
}

func TestResolveFiltersHiddenItems(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	store.Seed("u1",
		WorkItem{ID: "a", OwnerID: "u1", Meta: Metadata{Origin: OriginAgent}, GitHub: GitHubRef{UpdatedAt: now}, Signals: Signals{Reasons: []Reason{ReasonReviewRequested}}},
		WorkItem{ID: "b", OwnerID: "u1", Meta: Metadata{Origin: OriginAgent, HiddenAt: now}, GitHub: GitHubRef{UpdatedAt: now}, Signals: Signals{Reasons: []Reason{ReasonAuthor}}},
	)

	ing := &noopIngestor{}
	status, items, err := Resolve(context.Background(), store, ing, now, "u1", DefaultSort, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if status != StatusReady {
		t.Fatalf("status = %q, want ready", status)
	}
	if len(items) != 1 || items[0].ID != "a" {
		t.Fatalf("expected only the visible item 'a', got %+v", items)
	}
	if ing.called {
		t.Error("backfill must not run when the store has items (even if all hidden)")
	}
}
