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
		scored := Score(items[i], now)
		scored.UpdatedAt = items[i].Meta.UpdatedAt // preserve persisted write time
		items[i].Meta = scored
	}
	if err := Sort(items, key, desc); err != nil {
		return "", nil, err
	}
	return StatusReady, items, nil
}
