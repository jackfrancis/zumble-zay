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
}

func (f *fakePipeline) EnsureBackfill(context.Context, string) error {
	f.backfilled = true
	return nil
}
func (f *fakePipeline) Active(context.Context, string) (bool, error) { return f.active, nil }

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

func TestViewSelection(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	user := &session.User{ID: "u1"}

	t.Run("anonymous shows sign-in", func(t *testing.T) {
		h := handlerFor(nil, worklist.NewMemoryStore(), &fakePipeline{})
		if d, _ := h.view(req); d.View != "signin" {
			t.Errorf("view = %q, want signin", d.View)
		}
	})

	t.Run("active pipeline shows processing even with items", func(t *testing.T) {
		store := worklist.NewMemoryStore()
		store.Seed("u1", worklist.WorkItem{ID: "a", OwnerID: "u1", Meta: worklist.Metadata{Origin: worklist.OriginAgent}, GitHub: worklist.GitHubRef{UpdatedAt: time.Now()}})
		h := handlerFor(user, store, &fakePipeline{active: true})
		if d, _ := h.view(req); d.View != "processing" {
			t.Errorf("view = %q, want processing (don't show a half-ranked list)", d.View)
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
