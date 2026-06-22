package worklist

import (
	"testing"
	"time"
)

func item(id string, rank float64, p Priority, updated time.Time) WorkItem {
	return WorkItem{
		ID:   id,
		Meta: Metadata{Rank: rank, Priority: p},
		GitHub: GitHubRef{
			UpdatedAt: updated,
		},
	}
}

func ids(items []WorkItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSortByRankDescAndAsc(t *testing.T) {
	now := time.Now()
	mk := func() []WorkItem {
		return []WorkItem{
			item("a", 0.2, PriorityLow, now),
			item("b", 0.9, PriorityHigh, now),
			item("c", 0.5, PriorityMedium, now),
		}
	}

	desc := mk()
	if err := Sort(desc, SortRank, true); err != nil {
		t.Fatalf("Sort desc: %v", err)
	}
	if got := ids(desc); !equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("rank desc order = %v, want [b c a]", got)
	}

	asc := mk()
	if err := Sort(asc, SortRank, false); err != nil {
		t.Fatalf("Sort asc: %v", err)
	}
	if got := ids(asc); !equal(got, []string{"a", "c", "b"}) {
		t.Fatalf("rank asc order = %v, want [a c b]", got)
	}
}

func TestSortByPriority(t *testing.T) {
	now := time.Now()
	items := []WorkItem{
		item("low", 0, PriorityLow, now),
		item("high", 0, PriorityHigh, now),
		item("none", 0, PriorityNone, now),
		item("med", 0, PriorityMedium, now),
	}
	if err := Sort(items, SortPriority, true); err != nil {
		t.Fatalf("Sort: %v", err)
	}
	if got := ids(items); !equal(got, []string{"high", "med", "low", "none"}) {
		t.Fatalf("priority desc order = %v, want [high med low none]", got)
	}
}

func TestSortTieBreakByUpdated(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	// Same rank; newer GitHub update must come first.
	items := []WorkItem{
		item("old", 0.5, PriorityNone, older),
		item("new", 0.5, PriorityNone, newer),
	}
	if err := Sort(items, SortRank, true); err != nil {
		t.Fatalf("Sort: %v", err)
	}
	if got := ids(items); !equal(got, []string{"new", "old"}) {
		t.Fatalf("tie-break order = %v, want [new old]", got)
	}
}

func TestSortUnknownKey(t *testing.T) {
	if err := Sort(nil, SortKey("bogus"), true); err != ErrUnknownSort {
		t.Fatalf("expected ErrUnknownSort, got %v", err)
	}
}

func TestSortKeyValidAndDiscovery(t *testing.T) {
	if !SortRank.Valid() || SortKey("nope").Valid() {
		t.Fatal("Valid() returned wrong result")
	}
	keys := SortKeys()
	if len(keys) != 5 {
		t.Fatalf("expected 5 registered sort keys, got %d (%v)", len(keys), keys)
	}
}
