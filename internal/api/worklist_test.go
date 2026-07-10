package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// fakeIngestor records whether EnsureBackfill was called.
type fakeIngestor struct {
	calls int
	owner string
}

func (f *fakeIngestor) EnsureBackfill(_ context.Context, ownerID string) error {
	f.calls++
	f.owner = ownerID
	return nil
}

func authedRequest(target, ownerID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	p := &principal.Principal{
		Kind:         principal.KindUser,
		Subject:      ownerID,
		ActingUserID: ownerID,
		Scopes:       []principal.Scope{principal.ScopeAll},
	}
	return req.WithContext(principal.NewContext(req.Context(), p))
}

func decode(t *testing.T, body []byte) worklistResponse {
	t.Helper()
	var resp worklistResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestWorklistEmptyTriggersIngestion(t *testing.T) {
	store := worklist.NewMemoryStore()
	ing := &fakeIngestor{}
	h := NewWorklistHandler(store, ing)

	rec := httptest.NewRecorder()
	h.List(rec, authedRequest("/api/worklist", "u1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decode(t, rec.Body.Bytes())
	if resp.Status != "processing" {
		t.Fatalf("status = %q, want processing", resp.Status)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(resp.Items))
	}
	if ing.calls != 1 || ing.owner != "u1" {
		t.Fatalf("ingestor calls=%d owner=%q, want 1/u1", ing.calls, ing.owner)
	}
}

func TestWorklistReadyAndOrdered(t *testing.T) {
	now := time.Now()
	store := worklist.NewMemoryStore()
	// User-set metadata is preserved verbatim by the read path (not rescored),
	// so these explicit ranks drive the ordering assertion.
	store.Seed("u1",
		worklist.WorkItem{ID: "a", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.2, Origin: worklist.OriginUser}, GitHub: worklist.GitHubRef{UpdatedAt: now}},
		worklist.WorkItem{ID: "b", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.9, Origin: worklist.OriginUser}, GitHub: worklist.GitHubRef{UpdatedAt: now}},
		worklist.WorkItem{ID: "c", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.5, Origin: worklist.OriginUser}, GitHub: worklist.GitHubRef{UpdatedAt: now}},
	)
	ing := &fakeIngestor{}
	h := NewWorklistHandler(store, ing)

	rec := httptest.NewRecorder()
	h.List(rec, authedRequest("/api/worklist", "u1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decode(t, rec.Body.Bytes())
	if resp.Status != "ready" {
		t.Fatalf("status = %q, want ready", resp.Status)
	}
	got := []string{resp.Items[0].ID, resp.Items[1].ID, resp.Items[2].ID}
	if got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Fatalf("default order = %v, want [b c a]", got)
	}
	if ing.calls != 0 {
		t.Fatalf("ingestor should not be called when items exist; calls=%d", ing.calls)
	}
}

func TestWorklistInvalidSort(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("u1", worklist.WorkItem{ID: "a", OwnerID: "u1"})
	h := NewWorklistHandler(store, &fakeIngestor{})

	rec := httptest.NewRecorder()
	h.List(rec, authedRequest("/api/worklist?sort=bogus", "u1"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWorklistAscOrder(t *testing.T) {
	now := time.Now()
	store := worklist.NewMemoryStore()
	// User-set ranks are preserved by the read path, so they drive ordering.
	store.Seed("u1",
		worklist.WorkItem{ID: "a", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.2, Origin: worklist.OriginUser}, GitHub: worklist.GitHubRef{UpdatedAt: now}},
		worklist.WorkItem{ID: "b", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.9, Origin: worklist.OriginUser}, GitHub: worklist.GitHubRef{UpdatedAt: now}},
	)
	h := NewWorklistHandler(store, &fakeIngestor{})

	rec := httptest.NewRecorder()
	h.List(rec, authedRequest("/api/worklist?order=asc", "u1"))

	resp := decode(t, rec.Body.Bytes())
	if resp.Order != "asc" || resp.Items[0].ID != "a" {
		t.Fatalf("asc order wrong: order=%q first=%q", resp.Order, resp.Items[0].ID)
	}
}

func TestWorklistRescoresFromSignalsAtReadTime(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	store := worklist.NewMemoryStore()
	// Stored agent-derived, with populated Signals but stale/zero Meta: the read
	// path must derive the score from Signals as of the current clock.
	store.Seed("u1", worklist.WorkItem{
		ID: "pr1", OwnerID: "u1",
		Signals: worklist.Signals{
			Reasons:    []worklist.Reason{worklist.ReasonReviewRequested},
			DeadlineAt: base.Add(20 * 24 * time.Hour),
		},
		Meta:   worklist.Metadata{Origin: worklist.OriginAgent},
		GitHub: worklist.GitHubRef{UpdatedAt: base},
	})
	h := NewWorklistHandler(store, &fakeIngestor{})

	// Read "now": the stale zero rank must be replaced by a real score.
	h.now = func() time.Time { return base }
	rec := httptest.NewRecorder()
	h.List(rec, authedRequest("/api/worklist", "u1"))
	early := decode(t, rec.Body.Bytes()).Items[0].Meta
	if early.Rank == 0 {
		t.Fatalf("read-time rescore should populate rank, got 0")
	}

	// Read again with the deadline imminent: urgency (and rank) must rise.
	h.now = func() time.Time { return base.Add(19 * 24 * time.Hour) }
	rec = httptest.NewRecorder()
	h.List(rec, authedRequest("/api/worklist", "u1"))
	late := decode(t, rec.Body.Bytes()).Items[0].Meta
	if late.Urgency <= early.Urgency {
		t.Fatalf("urgency should rise as the deadline approaches: early=%v late=%v", early.Urgency, late.Urgency)
	}
}

// TestIngestPreservesAndClearsCompletedAt proves the sink treats the ingesting
// runtime as authoritative for completion: a stamped CompletedAt survives the
// rescore, and a later open re-ingest (a reopen) clears it so the item revives.
func TestIngestPreservesAndClearsCompletedAt(t *testing.T) {
	store := worklist.NewMemoryStore()
	h := NewIngestHandler(store, nil)
	done := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)

	post := func(it worklist.WorkItem) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"items": []worklist.WorkItem{it}})
		req := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
		p := &principal.Principal{Kind: principal.KindUser, Subject: "u1", ActingUserID: "u1", Scopes: []principal.Scope{principal.ScopeAll}}
		req = req.WithContext(principal.NewContext(req.Context(), p))
		rec := httptest.NewRecorder()
		h.Ingest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest status = %d, body=%s", rec.Code, rec.Body.String())
		}
	}

	// A runtime stamps a confirmed completion: the sink keeps it through Score.
	post(worklist.WorkItem{ID: "x", Source: "github", GitHub: worklist.GitHubRef{UpdatedAt: done}, Meta: worklist.Metadata{CompletedAt: done}})
	got, _ := store.List(context.Background(), "u1")
	if len(got) != 1 || got[0].Meta.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt should be preserved on ingest, got %+v", got)
	}

	// A later github-ingest re-fetches the same item as open (a reopen): the zero
	// CompletedAt in the payload clears it, so the item resurfaces.
	post(worklist.WorkItem{ID: "x", Source: "github", GitHub: worklist.GitHubRef{UpdatedAt: done.Add(time.Hour)}})
	got, _ = store.List(context.Background(), "u1")
	if len(got) != 1 || !got[0].Meta.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt should clear when re-ingested open, got %+v", got)
	}
}

// TestIngestPreservesProposedAcrossReingest proves the LLM axis proposal the
// llm-rank job writes survives a later github-ingest that carries none. Without
// preservation, the next ingest cycle's wholesale Upsert wipes Proposed and the
// item silently reverts to its signal-based rationale (docs/adr/0011).
func TestIngestPreservesProposedAcrossReingest(t *testing.T) {
	store := worklist.NewMemoryStore()
	h := NewIngestHandler(store, nil)
	updated := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)

	post := func(it worklist.WorkItem) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"items": []worklist.WorkItem{it}})
		req := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
		p := &principal.Principal{Kind: principal.KindUser, Subject: "u1", ActingUserID: "u1", Scopes: []principal.Scope{principal.ScopeAll}}
		req = req.WithContext(principal.NewContext(req.Context(), p))
		rec := httptest.NewRecorder()
		h.Ingest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest status = %d, body=%s", rec.Code, rec.Body.String())
		}
	}

	// The llm-rank job writes a proposal (with a rationale) for the item.
	post(worklist.WorkItem{
		ID: "x", Source: "github", GitHub: worklist.GitHubRef{UpdatedAt: updated},
		Signals: worklist.Signals{Proposed: &worklist.AxisProposal{
			Relevance: 0.9, Impact: 0.7, Engagement: 0.4, Urgency: 0.8, Confidence: 0.9,
			Rationale: "you own this review",
		}},
	})
	got, _ := store.List(context.Background(), "u1")
	if len(got) != 1 || got[0].Signals.Proposed == nil {
		t.Fatalf("proposal should be stored by the rank ingest, got %+v", got)
	}

	// A later github-ingest re-fetches the same item fresh from GitHub with no
	// proposal: the stored proposal (and its rationale) must survive.
	post(worklist.WorkItem{ID: "x", Source: "github", GitHub: worklist.GitHubRef{UpdatedAt: updated.Add(time.Hour)}})
	got, _ = store.List(context.Background(), "u1")
	if len(got) != 1 || got[0].Signals.Proposed == nil {
		t.Fatalf("proposal must survive a later github-ingest, got %+v", got)
	}
	if r := got[0].Signals.Proposed.Rationale; r != "you own this review" {
		t.Errorf("preserved rationale = %q, want the rank job's", r)
	}
	if got[0].Meta.Rationale != "you own this review" {
		t.Errorf("scored Meta.Rationale = %q, want the proposal's (not the signal default)", got[0].Meta.Rationale)
	}
}

// TestIngestFlagsBotRequestedReviews proves the bot-reviewer policy is applied in
// ZZ core: a review requested by a configured bot is flagged, a human's is not.
func TestIngestFlagsBotRequestedReviews(t *testing.T) {
	store := worklist.NewMemoryStore()
	h := NewIngestHandler(store, []string{"k8s-ci-robot"})

	post := func(id, requester string) worklist.WorkItem {
		t.Helper()
		it := worklist.WorkItem{ID: id, Source: "github", Signals: worklist.Signals{ReviewRequestedBy: requester}}
		body, _ := json.Marshal(map[string]any{"items": []worklist.WorkItem{it}})
		req := httptest.NewRequest(http.MethodPost, "/agent/worklist", bytes.NewReader(body))
		p := &principal.Principal{Kind: principal.KindUser, Subject: "u1", ActingUserID: "u1", Scopes: []principal.Scope{principal.ScopeAll}}
		req = req.WithContext(principal.NewContext(req.Context(), p))
		rec := httptest.NewRecorder()
		h.Ingest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest status = %d, body=%s", rec.Code, rec.Body.String())
		}
		got, _ := store.List(context.Background(), "u1")
		for _, w := range got {
			if w.ID == id {
				return w
			}
		}
		t.Fatalf("item %s not stored", id)
		return worklist.WorkItem{}
	}

	if w := post("bot", "k8s-ci-robot"); !w.Signals.ReviewRequestedByBot {
		t.Errorf("a bot-requested review should be flagged, got %+v", w.Signals)
	}
	if w := post("human", "alice"); w.Signals.ReviewRequestedByBot {
		t.Errorf("a human-requested review must not be flagged, got %+v", w.Signals)
	}
}

func TestAgentWorklistListReturnsStoredItemsRaw(t *testing.T) {
	store := worklist.NewMemoryStore()
	// Stored with a non-trivial rank; the agent read must return it verbatim,
	// without the read-time rescore that GET /api/worklist applies.
	store.Seed("u1", worklist.WorkItem{
		ID: "x", OwnerID: "u1",
		Signals: worklist.Signals{Reasons: []worklist.Reason{worklist.ReasonReviewRequested}},
		Meta:    worklist.Metadata{Rank: 0.42, Origin: worklist.OriginAgent},
	})
	h := NewIngestHandler(store, nil)

	rec := httptest.NewRecorder()
	h.List(rec, authedRequest("/agent/worklist", "u1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Items []worklist.WorkItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "x" {
		t.Fatalf("unexpected items: %+v", body.Items)
	}
	if body.Items[0].Meta.Rank != 0.42 {
		t.Errorf("agent read must not rescore: rank=%v want 0.42", body.Items[0].Meta.Rank)
	}
}

func TestAgentWorklistListLimitReturnsTopByRank(t *testing.T) {
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "low", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.1, Origin: worklist.OriginAgent}},
		worklist.WorkItem{ID: "high", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.9, Origin: worklist.OriginAgent}},
		worklist.WorkItem{ID: "mid", OwnerID: "u1", Meta: worklist.Metadata{Rank: 0.5, Origin: worklist.OriginAgent}},
	)
	h := NewIngestHandler(store, nil)

	rec := httptest.NewRecorder()
	h.List(rec, authedRequest("/agent/worklist?limit=2", "u1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Items []worklist.WorkItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("limit=2 should cap to 2 items, got %d", len(body.Items))
	}
	if body.Items[0].ID != "high" || body.Items[1].ID != "mid" {
		t.Errorf("expected top-2 by rank [high mid], got [%s %s]", body.Items[0].ID, body.Items[1].ID)
	}
}
