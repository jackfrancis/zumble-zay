package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

type fakeSessions struct{ user *session.User }

func (f fakeSessions) CurrentUser(*http.Request) *session.User { return f.user }

type fakePipeline struct {
	active     bool
	backfilled bool
	conversed  []string
}

func (f *fakePipeline) EnsureBackfill(context.Context, string) error {
	f.backfilled = true
	return nil
}
func (f *fakePipeline) Active(context.Context, string) (bool, error) { return f.active, nil }
func (f *fakePipeline) Converse(_ context.Context, _, itemID string) error {
	f.conversed = append(f.conversed, itemID)
	return nil
}

type fakeProviders struct{}

func (fakeProviders) Providers() []string { return []string{"github"} }

func handlerFor(user *session.User, store worklist.Store, pipe *fakePipeline) *Handler {
	return New(fakeSessions{user}, store, pipe, fakeProviders{}, true)
}

func TestWorklistFlagsItemsWithDiscussion(t *testing.T) {
	user := &session.User{ID: "u1"}
	store := worklist.NewMemoryStore()
	now := time.Now()
	store.Seed("u1",
		worklist.WorkItem{
			ID: "a", OwnerID: "u1", Meta: worklist.Metadata{Origin: worklist.OriginAgent},
			GitHub: worklist.GitHubRef{Number: 1, UpdatedAt: now},
			Thread: []worklist.Message{{Role: worklist.RoleUser, Content: "hi"}, {Role: worklist.RoleAgent, Content: "hello"}},
		},
		worklist.WorkItem{
			ID: "b", OwnerID: "u1", Meta: worklist.Metadata{Origin: worklist.OriginAgent},
			GitHub: worklist.GitHubRef{Number: 2, UpdatedAt: now},
		},
	)
	h := handlerFor(user, store, &fakePipeline{active: false})

	rec := httptest.NewRecorder()
	h.Index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// Exactly the threaded item gets the emphasized cue; the other stays plain.
	if n := strings.Count(body, "zz-discuss--active"); n != 1 {
		t.Errorf("expected exactly one flagged Discuss button, got %d", n)
	}
	if !strings.Contains(body, "&#10024; Discuss") {
		t.Errorf("expected the sparkle cue on the discussed item")
	}
	if n := strings.Count(body, ">Discuss<"); n != 1 {
		t.Errorf("expected exactly one plain Discuss button, got %d", n)
	}
}

func TestReviewPRsSeedsAndEnqueuesVisiblePRs(t *testing.T) {
	user := &session.User{ID: "u1"}
	store := worklist.NewMemoryStore()
	store.Seed("u1",
		worklist.WorkItem{ID: "pr1", OwnerID: "u1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 1}},
		worklist.WorkItem{ID: "iss1", OwnerID: "u1", Type: worklist.TypeIssue, GitHub: worklist.GitHubRef{Number: 2}},
		worklist.WorkItem{ID: "pr2", OwnerID: "u1", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Number: 3}, Meta: worklist.Metadata{HiddenAt: time.Now()}},
	)
	pipe := &fakePipeline{}
	h := handlerFor(user, store, pipe)

	rec := httptest.NewRecorder()
	h.ReviewPRs(rec, httptest.NewRequest(http.MethodPost, "/review-prs", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	// Only the visible PR is reviewed: the issue and the hidden PR are skipped.
	if len(pipe.conversed) != 1 || pipe.conversed[0] != "pr1" {
		t.Fatalf("conversed = %v, want [pr1]", pipe.conversed)
	}
	items, _ := store.List(context.Background(), "u1")
	for _, it := range items {
		switch it.ID {
		case "pr1":
			if n := len(it.Thread); n != 1 || it.Thread[0].Role != worklist.RoleUser || it.Thread[0].Content != "Can you review this PR?" {
				t.Fatalf("pr1 thread = %+v, want one user review turn", it.Thread)
			}
		case "iss1", "pr2":
			if len(it.Thread) != 0 {
				t.Fatalf("%s should not be seeded", it.ID)
			}
		}
	}
}

func TestViewSelection(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	user := &session.User{ID: "u1"}

	t.Run("anonymous shows sign-in", func(t *testing.T) {
		h := handlerFor(nil, worklist.NewMemoryStore(), &fakePipeline{})
		if d, _ := h.view(req); d.View != "signin" {
			t.Errorf("view = %q, want signin", d.View)
		}
	})

	t.Run("active pass keeps the worklist visible and auto-refreshes", func(t *testing.T) {
		store := worklist.NewMemoryStore()
		store.Seed("u1", worklist.WorkItem{ID: "a", OwnerID: "u1", Meta: worklist.Metadata{Origin: worklist.OriginAgent}, GitHub: worklist.GitHubRef{UpdatedAt: time.Now()}})
		h := handlerFor(user, store, &fakePipeline{active: true})
		d, _ := h.view(req)
		if d.View != "worklist" {
			t.Fatalf("view = %q, want worklist (a populated list must never blank to Discovering mid-refresh)", d.View)
		}
		if len(d.Items) == 0 {
			t.Error("the existing items must stay on screen during a refresh pass")
		}
		if d.RefreshSecs == 0 {
			t.Error("an active pass should auto-refresh the worklist so the new ranking lands")
		}
	})

	t.Run("settled with items shows the worklist (static)", func(t *testing.T) {
		store := worklist.NewMemoryStore()
		store.Seed("u1", worklist.WorkItem{ID: "a", OwnerID: "u1", Meta: worklist.Metadata{Origin: worklist.OriginAgent}, GitHub: worklist.GitHubRef{UpdatedAt: time.Now()}})
		h := handlerFor(user, store, &fakePipeline{active: false})
		d, _ := h.view(req)
		if d.View != "worklist" {
			t.Fatalf("view = %q, want worklist", d.View)
		}
		if d.RefreshSecs != 0 {
			t.Errorf("settled worklist should not auto-refresh, got %d", d.RefreshSecs)
		}
	})

	t.Run("settled but empty triggers backfill and shows processing", func(t *testing.T) {
		pipe := &fakePipeline{active: false}
		h := handlerFor(user, worklist.NewMemoryStore(), pipe)
		if d, _ := h.view(req); d.View != "processing" {
			t.Errorf("view = %q, want processing", d.View)
		}
		if !pipe.backfilled {
			t.Error("empty store should trigger a backfill")
		}
	})
}
