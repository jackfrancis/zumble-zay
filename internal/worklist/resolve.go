package worklist

import (
	"context"
	"time"
)

// Worklist read statuses shared by the JSON API and the HTML UI.
const (
	StatusReady      = "ready"      // items are returned
	StatusProcessing = "processing" // empty, so a backfill was triggered
)

// Resolve is the shared read model for a user's worklist, used by both the JSON
// API and the HTML UI so the two cannot drift. It loads the owner's items, and:
//
//   - if there are none, triggers an idempotent backfill and returns
//     StatusProcessing with no items;
//   - otherwise rescores agent-derived items against now (time-dependent axes
//     decay between writes), preserving human overrides (OriginUser), sorts by
//     key/desc, and returns StatusReady.
//
// Rescoring at read time keeps urgency/engagement fresh without re-fetching from
// the provider (docs/adr/0008).
func Resolve(ctx context.Context, store Store, ingestor Ingestor, now time.Time, ownerID string, key SortKey, desc bool) (string, []WorkItem, error) {
	items, err := store.List(ctx, ownerID)
	if err != nil {
		return "", nil, err
	}
	if len(items) == 0 {
		if err := ingestor.EnsureBackfill(ctx, ownerID); err != nil {
			return "", nil, err
		}
		return StatusProcessing, nil, nil
	}
	for i := range items {
		if items[i].Meta.Origin == OriginUser {
			continue // human overrides are preserved verbatim
		}
		hidden := items[i].Meta.HiddenAt       // survives the rescore below
		completed := items[i].Meta.CompletedAt // survives the rescore below
		scored := Score(items[i], now)
		scored.UpdatedAt = items[i].Meta.UpdatedAt // preserve persisted write time
		scored.HiddenAt = hidden
		scored.CompletedAt = completed
		items[i].Meta = scored
	}
	// Hidden items (user-set) and completed items (closed/merged, agent-set) stay
	// in the store so an agent can still see and revive them, but are dropped from
	// the user-facing list (docs/adr/0017).
	visible := items[:0]
	for _, it := range items {
		if it.Meta.HiddenAt.IsZero() && it.Meta.CompletedAt.IsZero() {
			visible = append(visible, it)
		}
	}
	items = visible
	if err := Sort(items, key, desc); err != nil {
		return "", nil, err
	}
	return StatusReady, items, nil
}

// HiddenAfter returns the HiddenAt to keep for a re-ingested item: it preserves a
// prior hidden timestamp, but clears it (auto-unhide) when the underlying item
// has been updated since it was hidden, so a changed item resurfaces. updatedAt
// is the GitHub item's updated_at (docs/adr/0017).
func HiddenAfter(prevHiddenAt, updatedAt time.Time) time.Time {
	if prevHiddenAt.IsZero() || updatedAt.After(prevHiddenAt) {
		return time.Time{}
	}
	return prevHiddenAt
}
