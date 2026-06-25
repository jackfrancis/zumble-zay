package api

import (
	"encoding/json"
	"net/http"
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

	now := time.Now().UTC()
	for i := range req.Items {
		// Multi-tenant isolation: an agent only ever writes for its acting user.
		req.Items[i].OwnerID = p.ActingUserID
		// Provenance: agent writes are always agent-derived; humans override.
		req.Items[i].Meta.Origin = worklist.OriginAgent
		// Decorate ZZ metadata by scoring the item's signals (docs/adr/0008).
		req.Items[i].Meta = worklist.Score(req.Items[i], now)
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
