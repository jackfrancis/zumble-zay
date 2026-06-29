package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// maxIngestBytes bounds an ingestion request body.
const maxIngestBytes = 4 << 20 // 4 MiB

// IngestHandler is the agent output sink. An ephemeral runtime that fetched a
// user's source data posts normalized WorkItems here; ZZ stamps provenance and
// persists them. This is the write counterpart to the read-only GET
// /api/worklist (see docs/adr/0006).
type IngestHandler struct {
	store worklist.Store
}

// NewIngestHandler constructs an IngestHandler over the given store.
func NewIngestHandler(store worklist.Store) *IngestHandler {
	return &IngestHandler{store: store}
}

type ingestRequest struct {
	Items []worklist.WorkItem `json:"items"`
}

// Ingest handles POST /agent/worklist. It requires the metadata:write scope.
// Items are force-scoped to the acting user and marked agent-derived, so a
// runtime can neither write another user's data nor masquerade as a human edit.
func (h *IngestHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req ingestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no items to ingest")
		return
	}

	// Preserve any prior "hidden" state across re-ingest, auto-unhiding an item
	// once GitHub shows it changed after it was hidden (docs/adr/0017). Reading
	// the existing items here is the merge point for that user-set metadata.
	existing, err := h.store.List(r.Context(), p.ActingUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load work items")
		return
	}
	prevHidden := make(map[string]time.Time, len(existing))
	prevThread := make(map[string][]worklist.Message, len(existing))
	prevResearch := make(map[string]*worklist.ResearchAdjustment, len(existing))
	for _, it := range existing {
		if !it.Meta.HiddenAt.IsZero() {
			prevHidden[it.ID] = it.Meta.HiddenAt
		}
		if len(it.Thread) > 0 {
			prevThread[it.ID] = it.Thread
		}
		if it.Signals.Research != nil {
			prevResearch[it.ID] = it.Signals.Research
		}
	}

	now := time.Now().UTC()
	for i := range req.Items {
		// Multi-tenant isolation: an agent only ever writes for its acting user.
		req.Items[i].OwnerID = p.ActingUserID
		// Provenance: agent writes are always agent-derived; humans override.
		req.Items[i].Meta.Origin = worklist.OriginAgent
		// Preserve the discussion-derived research re-weighting across re-ingest
		// (docs/adr/0022), BEFORE scoring so it re-applies to the refreshed
		// foundation. A github-ingest sends no research; enrich/rank round-trip the
		// stored value, so this is a no-op for them.
		req.Items[i].Signals.Research = prevResearch[req.Items[i].ID]
		// Decorate ZZ metadata by scoring the item's signals (docs/adr/0008).
		req.Items[i].Meta = worklist.Score(req.Items[i], now)
		// Carry (or auto-clear) the user's hidden state (docs/adr/0017).
		req.Items[i].Meta.HiddenAt = worklist.HiddenAfter(prevHidden[req.Items[i].ID], req.Items[i].GitHub.UpdatedAt)
		// Preserve the assistive conversation across re-ingest (docs/adr/0018);
		// agents never author it, so the stored thread is authoritative.
		req.Items[i].Thread = prevThread[req.Items[i].ID]
		if req.Items[i].CreatedAt.IsZero() {
			req.Items[i].CreatedAt = now
		}
		req.Items[i].UpdatedAt = now
		req.Items[i].Meta.UpdatedAt = now
	}

	if err := h.store.Upsert(r.Context(), p.ActingUserID, req.Items...); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist items")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ingested": len(req.Items)})
}

// List handles GET /agent/worklist. It returns the acting user's persisted work
// items verbatim — no read-time rescore and no backfill-on-empty — so an
// enrichment runtime can augment stored items in place rather than re-deriving
// them from the provider (docs/adr/0010). Multi-tenant isolation: a workload
// only ever sees its acting user's items.
func (h *IngestHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	items, err := h.store.List(r.Context(), p.ActingUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load work items")
		return
	}
	// Optional single-item fetch: a per-item runtime (github-converse) reads just
	// the item it was dispatched for, rather than the whole worklist (docs/adr/0019).
	if id := r.URL.Query().Get("id"); id != "" {
		out := []worklist.WorkItem{}
		for _, it := range items {
			if it.ID == id {
				out = append(out, it)
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
		return
	}
	// Optional shortlist: when limit > 0, return the top-N by rank so an
	// enrichment runtime can bound its expensive per-item fan-out (docs/adr/0010).
	// Selection lives here because ZZ owns ranking; the agent only chooses depth.
	if limit := parseLimit(r.URL.Query().Get("limit")); limit > 0 && limit < len(items) {
		_ = worklist.Sort(items, worklist.SortRank, true)
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func parseLimit(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
