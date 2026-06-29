package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/markdown"
	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// maxMessageBytes bounds a single user chat message.
const maxMessageBytes = 16 << 10 // 16 KiB

// maxReplyBytes bounds an agent reply written back through the agent plane. A
// drafted comment can be long, so it is more generous than a user message.
const maxReplyBytes = 64 << 10 // 64 KiB

// ConverseEnqueuer schedules an asynchronous assistant turn for one item. The
// orchestrator implements it by spawning an ephemeral converse runtime that
// gathers live GitHub context and writes the reply back (docs/adr/0019). It is
// the seam that keeps the HTTP layer from importing the runtime substrate.
type ConverseEnqueuer interface {
	Converse(ctx context.Context, ownerID, itemID string) error
}

// ThreadHandler powers the per-item assistive conversation. POST schedules an
// assistant turn (handled out-of-process by a converse runtime); GET returns the
// current thread for the page to poll. It is read-only with respect to GitHub:
// ZZ only ever appends messages, never acts on the provider (docs/adr/0018, 0019).
type ThreadHandler struct {
	store    worklist.Store
	enqueuer ConverseEnqueuer
	enabled  bool
	now      func() time.Time
}

// NewThreadHandler constructs a ThreadHandler. When enabled is false (no model
// configured) POST reports the assistant as unavailable, but GET still serves
// the stored thread.
func NewThreadHandler(store worklist.Store, enqueuer ConverseEnqueuer, enabled bool) *ThreadHandler {
	return &ThreadHandler{store: store, enqueuer: enqueuer, enabled: enabled, now: time.Now}
}

type postMessageRequest struct {
	Content string `json:"content"`
}

// messageView is a thread message prepared for the client: the raw content plus
// its Markdown rendered to sanitized HTML, so the page can display formatted
// replies without rendering Markdown itself (docs/adr/0021).
type messageView struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	HTML    string    `json:"html"`
	At      time.Time `json:"at"`
}

type threadResponse struct {
	Messages []messageView `json:"messages"`
}

// toMessageViews renders each agent message's Markdown to sanitized HTML for
// display. User messages stay plain text (the page never renders them as HTML),
// so their content is shown verbatim.
func toMessageViews(msgs []worklist.Message) []messageView {
	out := make([]messageView, len(msgs))
	for i, m := range msgs {
		v := messageView{Role: m.Role, Content: m.Content, At: m.At}
		if m.Role == worklist.RoleAgent {
			v.HTML = markdown.ToSafeHTMLString(m.Content)
		}
		out[i] = v
	}
	return out
}

// Post handles POST /api/thread?id=<item id>. The item id rides in the query
// (URL-encoded) because item ids contain '/' and '#'. It is owner-scoped: a user
// can only converse about their own items. It appends the user's message and
// schedules an assistant turn, returning 202 with the user message; the reply
// arrives asynchronously and the page polls Get for it (docs/adr/0019).
func (h *ThreadHandler) Post(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !h.enabled || h.enqueuer == nil {
		writeError(w, http.StatusServiceUnavailable, "assistant is not configured")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "item id required")
		return
	}

	var req postMessageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessageBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	text := strings.TrimSpace(req.Content)
	if text == "" {
		writeError(w, http.StatusBadRequest, "message content required")
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

	// Append the user's turn first, so the spawned runtime reads it from the
	// stored thread — the message never rides the job's environment.
	userMsg := worklist.Message{Role: worklist.RoleUser, Content: text, At: h.now().UTC()}
	item.Thread = append(item.Thread, userMsg)
	if err := h.store.Upsert(r.Context(), p.ActingUserID, item); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the message")
		return
	}
	if err := h.enqueuer.Converse(r.Context(), p.ActingUserID, id); err != nil {
		writeError(w, http.StatusBadGateway, "could not start the assistant")
		return
	}
	writeJSON(w, http.StatusAccepted, threadResponse{Messages: toMessageViews([]worklist.Message{userMsg})})
}

// Resume re-ensures an assistant turn for an item whose last message is an
// unanswered user turn — e.g. the page was reopened after the converse runtime
// crashed and the reply never arrived. It appends nothing; it just re-enqueues,
// which the orchestrator dedups against any still-running turn, so a healthy turn
// is a no-op and a failed one self-heals on revisit. Returns 202 when a turn is
// (re)started, or 200 when there is nothing pending to resume.
func (h *ThreadHandler) Resume(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !h.enabled || h.enqueuer == nil {
		writeError(w, http.StatusServiceUnavailable, "assistant is not configured")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "item id required")
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
	// Only an unanswered user turn needs resuming; otherwise there is no work.
	if n := len(item.Thread); n == 0 || item.Thread[n-1].Role != worklist.RoleUser {
		writeJSON(w, http.StatusOK, map[string]any{"pending": false})
		return
	}
	if err := h.enqueuer.Converse(r.Context(), p.ActingUserID, id); err != nil {
		writeError(w, http.StatusBadGateway, "could not start the assistant")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true})
}

// Get handles GET /api/thread?id=<item id>. It returns the owner's current
// thread for the item so the page can poll for the asynchronous reply.
func (h *ThreadHandler) Get(w http.ResponseWriter, r *http.Request) {
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
	item, found, err := findItem(r.Context(), h.store, p.ActingUserID, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load item")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "item not found")
		return
	}
	writeJSON(w, http.StatusOK, threadResponse{Messages: toMessageViews(item.Thread)})
}

// AgentThreadHandler is the agent-plane write-back for the conversation: a
// converse runtime posts the assistant's reply here, and ZZ appends it to the
// item's thread (docs/adr/0019). It is the per-item counterpart to the ingest
// sink and requires the metadata:write scope (enforced at the route).
type AgentThreadHandler struct {
	store worklist.Store
	now   func() time.Time
}

// NewAgentThreadHandler constructs an AgentThreadHandler over the given store.
func NewAgentThreadHandler(store worklist.Store) *AgentThreadHandler {
	return &AgentThreadHandler{store: store, now: time.Now}
}

// Append handles POST /agent/thread?id=<item id>. The reply is force-attributed
// to the agent and scoped to the runtime's acting user, so a runtime can neither
// write another user's data nor masquerade as the human.
func (h *AgentThreadHandler) Append(w http.ResponseWriter, r *http.Request) {
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

	var req postMessageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReplyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	text := strings.TrimSpace(req.Content)
	if text == "" {
		writeError(w, http.StatusBadRequest, "message content required")
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
	item.Thread = append(item.Thread, worklist.Message{Role: worklist.RoleAgent, Content: text, At: h.now().UTC()})
	if err := h.store.Upsert(r.Context(), p.ActingUserID, item); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the reply")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"appended": true})
}

// findItem returns the owner's work item with the given id, if present.
func findItem(ctx context.Context, store worklist.Store, ownerID, id string) (worklist.WorkItem, bool, error) {
	items, err := store.List(ctx, ownerID)
	if err != nil {
		return worklist.WorkItem{}, false, err
	}
	for _, it := range items {
		if it.ID == id {
			return it, true, nil
		}
	}
	return worklist.WorkItem{}, false, nil
}
