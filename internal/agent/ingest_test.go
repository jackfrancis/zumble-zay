package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/github"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// TestRetireCompleted proves the ingest runtime retires only work that has both
// left the open snapshot AND is confirmed closed/merged: an item still open (e.g.
// merely unassigned) is kept, and an item still in the snapshot is never checked.
func TestRetireCompleted(t *testing.T) {
	// GitHub item state: #2 is closed, #3 is still open. #1 must never be queried
	// (it is in the open snapshot, so known open).
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/2":
			_, _ = w.Write([]byte(`{"state":"closed","closed_at":"2026-06-20T10:00:00Z"}`))
		case "/repos/o/r/issues/3":
			_, _ = w.Write([]byte(`{"state":"open"}`))
		default:
			t.Errorf("unexpected state query: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer gh.Close()

	// Stored worklist: #1 (open), #2 (dropped off, closed), #3 (dropped off, still
	// open), and a done #4 that must be skipped (already retired).
	closed := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	stored := []worklist.WorkItem{
		{ID: "github:o/r#1", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1}},
		{ID: "github:o/r#2", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 2}},
		{ID: "github:o/r#3", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 3}},
		{ID: "github:o/r#4", Source: "github", Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "o/r", Number: 4}, Meta: worklist.Metadata{CompletedAt: closed}},
	}
	zzSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/worklist" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": stored})
			return
		}
		t.Errorf("unexpected ZZ call: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	defer zzSrv.Close()

	ghClient := github.NewClient(gh.Client(), gh.URL)
	zz := NewZZClient(zzSrv.URL, "tok", zzSrv.Client())

	// The fresh open snapshot contains only #1.
	open := []worklist.WorkItem{stored[0]}
	retired := retireCompleted(context.Background(), ghClient, "tok", zz, open)

	if len(retired) != 1 {
		t.Fatalf("want exactly one retired item (#2), got %d: %+v", len(retired), retired)
	}
	if retired[0].ID != "github:o/r#2" {
		t.Errorf("retired the wrong item: %s", retired[0].ID)
	}
	if retired[0].Meta.CompletedAt.IsZero() {
		t.Errorf("retired item must carry CompletedAt")
	}
}
