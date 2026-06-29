package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// maxResearchBytes bounds a research write-back body.
const maxResearchBytes = 8 << 10

// AgentResearchHandler is the agent-plane write-back for the research re-ranking:
// a github-research runtime posts the per-axis multipliers here, and ZZ applies
// them to the item's foundation and re-scores it (docs/adr/0022). It requires the
// metadata:write scope (enforced at the route).
type AgentResearchHandler struct {
	store worklist.Store
	now   func() time.Time
}

// NewAgentResearchHandler constructs an AgentResearchHandler over the given store.
func NewAgentResearchHandler(store worklist.Store) *AgentResearchHandler {
	return &AgentResearchHandler{store: store, now: time.Now}
}

// Set handles POST /agent/research?id=<item id>. The adjustment is force-scoped
// to the runtime's acting user; ZZ stores it on the item and re-scores so the
// multipliers take effect immediately, preserving the user's hidden state.
func (h *AgentResearchHandler) Set(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "item id required")
		return
	}

	var adj worklist.ResearchAdjustment
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResearchBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&adj); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	item, found, err := findItem(r.Context(), h.store, p.ActingUserID, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load item")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "item not found")
		return
	}

	now := h.now().UTC()
	hidden := item.Meta.HiddenAt // preserve user-set state across the re-score
	item.Signals.Research = &adj
	item.Meta = worklist.Score(item, now)
	item.Meta.HiddenAt = hidden
	item.Meta.Origin = worklist.OriginAgent
	item.Meta.UpdatedAt = now
	item.UpdatedAt = now
	if err := h.store.Upsert(r.Context(), p.ActingUserID, item); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the research")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": true})
}
