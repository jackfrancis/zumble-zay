package api

import (
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
