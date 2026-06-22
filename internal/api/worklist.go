// Package api implements the JSON HTTP handlers for Zumble-Zay resources.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// WorklistHandler serves the user's ordered set of work.
type WorklistHandler struct {
	store    worklist.Store
	ingestor worklist.Ingestor
}

// NewWorklistHandler constructs a WorklistHandler.
func NewWorklistHandler(store worklist.Store, ingestor worklist.Ingestor) *WorklistHandler {
	return &WorklistHandler{store: store, ingestor: ingestor}
}

// worklistResponse is the envelope returned by List.
type worklistResponse struct {
	// Status is "ready" when items are returned, or "processing" when the work
	// list was empty and an ingestion/backfill has been kicked off.
	Status string              `json:"status"`
	Sort   string              `json:"sort"`
	Order  string              `json:"order"`
	Items  []worklist.WorkItem `json:"items"`
}

// List handles GET /api/worklist. It returns the authenticated owner's work
// items ordered by the requested sort. An empty result is expected to be rare
// (data is persisted); when it happens, List triggers an idempotent ingestion
// and reports status "processing" so the UI can show an engaging waiting state.
//
// Query params:
//
//	sort  = rank | priority | impact | relevance | updated   (default: rank)
//	order = desc | asc                                        (default: desc)
func (h *WorklistHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	key := worklist.SortKey(r.URL.Query().Get("sort"))
	if key == "" {
		key = worklist.DefaultSort
	}
	desc, err := parseOrder(r.URL.Query().Get("order"))
	if err != nil || !key.Valid() {
		writeError(w, http.StatusBadRequest, "invalid sort or order")
		return
	}

	items, err := h.store.List(r.Context(), p.ActingUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load work items")
		return
	}

	if len(items) == 0 {
		// Rare with persistence: start the agentic backfill and tell the UI to
		// show its waiting experience.
		if err := h.ingestor.EnsureBackfill(r.Context(), p.ActingUserID); err != nil {
			writeError(w, http.StatusBadGateway, "could not start processing")
			return
		}
		writeJSON(w, http.StatusOK, worklistResponse{
			Status: "processing",
			Sort:   string(key),
			Order:  orderString(desc),
			Items:  []worklist.WorkItem{},
		})
		return
	}

	if err := worklist.Sort(items, key, desc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid sort")
		return
	}
	writeJSON(w, http.StatusOK, worklistResponse{
		Status: "ready",
		Sort:   string(key),
		Order:  orderString(desc),
		Items:  items,
	})
}

var errBadOrder = errors.New("invalid order")

func parseOrder(s string) (desc bool, err error) {
	switch s {
	case "", "desc":
		return true, nil
	case "asc":
		return false, nil
	default:
		return false, errBadOrder
	}
}

func orderString(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
