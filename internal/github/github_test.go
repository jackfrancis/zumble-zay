package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// searchBody includes two PRs and one plain issue (no pull_request). The issue
// must be filtered out, and the PRs deduped across the three signal queries.
const searchBody = `{"items":[
  {"number":1,"title":"Fix bug","html_url":"https://github.com/octo/repo/pull/1","state":"open","created_at":"2026-06-19T10:00:00Z","updated_at":"2026-06-20T10:00:00Z","comments":5,"repository_url":"https://api.github.com/repos/octo/repo","labels":[{"name":"sig/foo"},{"name":"kind/bug"}],"milestone":{"due_on":"2026-07-01T00:00:00Z"},"reactions":{"total_count":7},"pull_request":{"url":"https://api.github.com/repos/octo/repo/pulls/1"}},
  {"number":2,"title":"Add feature","html_url":"https://github.com/octo/repo/pull/2","state":"open","updated_at":"2026-06-21T10:00:00Z","repository_url":"https://api.github.com/repos/octo/repo","pull_request":{"url":"https://api.github.com/repos/octo/repo/pulls/2"}},
  {"number":3,"title":"A plain issue","html_url":"https://github.com/octo/repo/issues/3","state":"open","updated_at":"2026-06-22T10:00:00Z","repository_url":"https://api.github.com/repos/octo/repo"}
]}`

func TestFetchWorklistMapsFiltersAndDedupes(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		calls++
		_, _ = w.Write([]byte(searchBody))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	items, err := c.FetchWorklist(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchWorklist: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 signal queries, got %d", calls)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 deduped PRs (issue filtered), got %d", len(items))
	}

	ids := make(map[string]worklist.WorkItem, len(items))
	for _, it := range items {
		ids[it.ID] = it
		if it.Type != worklist.TypePullRequest {
			t.Errorf("non-PR leaked through: %+v", it)
		}
		if it.GitHub.Repo != "octo/repo" {
			t.Errorf("repo not parsed from repository_url: %q", it.GitHub.Repo)
		}
		if it.Meta.Origin != worklist.OriginAgent {
			t.Errorf("origin %q != %q", it.Meta.Origin, worklist.OriginAgent)
		}
	}
	if _, ok := ids["github:octo/repo#1"]; !ok {
		t.Errorf("missing PR #1; got ids %v", ids)
	}
	if _, ok := ids["github:octo/repo#2"]; !ok {
		t.Errorf("missing PR #2; got ids %v", ids)
	}

	// PR #1 carries the cheap signals mapped straight from the search response.
	pr1 := ids["github:octo/repo#1"]
	// The stub returns the same body for all three queries, so PR #1 matches
	// author, assignee, and review-requested: the reasons merge onto one item.
	if len(pr1.Signals.Reasons) != 3 {
		t.Errorf("expected 3 merged reasons, got %v", pr1.Signals.Reasons)
	}
	if !hasReason(pr1.Signals.Reasons, worklist.ReasonReviewRequested) {
		t.Errorf("missing review-requested reason: %v", pr1.Signals.Reasons)
	}
	if pr1.Signals.Comments != 5 {
		t.Errorf("comments = %d, want 5", pr1.Signals.Comments)
	}
	if pr1.Signals.Reactions != 7 {
		t.Errorf("reactions = %d, want 7", pr1.Signals.Reactions)
	}
	if got := pr1.Signals.Labels; len(got) != 2 || got[0] != "sig/foo" || got[1] != "kind/bug" {
		t.Errorf("labels = %v, want [sig/foo kind/bug]", got)
	}
	if pr1.Signals.OpenedAt.IsZero() {
		t.Errorf("opened_at not mapped from created_at")
	}
	if pr1.Signals.DeadlineAt.IsZero() {
		t.Errorf("deadline_at not mapped from milestone due_on")
	}
	if pr1.Signals.ObservedAt.IsZero() {
		t.Errorf("observed_at not stamped")
	}
}

func hasReason(rs []worklist.Reason, r worklist.Reason) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func TestFetchWorklistPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	if _, err := c.FetchWorklist(context.Background(), "tok"); err == nil {
		t.Fatal("expected an error when GitHub returns non-200")
	}
}

func TestLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	login, err := c.Login(context.Background(), "tok")
	if err != nil || login != "octocat" {
		t.Fatalf("Login = %q, %v", login, err)
	}
}

func TestAwaitingMeSince(t *testing.T) {
	const me = "octocat"
	cases := []struct {
		name     string
		body     string
		wantZero bool
	}{
		{
			name:     "requested and not yet reviewed",
			body:     `[{"event":"review_requested","created_at":"2026-06-20T10:00:00Z","requested_reviewer":{"login":"octocat"}}]`,
			wantZero: false,
		},
		{
			name:     "reviewed after the request",
			body:     `[{"event":"review_requested","created_at":"2026-06-20T10:00:00Z","requested_reviewer":{"login":"octocat"}},{"event":"reviewed","submitted_at":"2026-06-21T10:00:00Z","user":{"login":"octocat"}}]`,
			wantZero: true,
		},
		{
			name:     "requested of someone else",
			body:     `[{"event":"review_requested","created_at":"2026-06-20T10:00:00Z","requested_reviewer":{"login":"someone-else"}}]`,
			wantZero: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClient(srv.Client(), srv.URL)
			at, err := c.AwaitingMeSince(context.Background(), "tok", "octo/repo", 1, me)
			if err != nil {
				t.Fatalf("AwaitingMeSince: %v", err)
			}
			if at.IsZero() != tc.wantZero {
				t.Fatalf("zero=%v, want %v (at=%v)", at.IsZero(), tc.wantZero, at)
			}
		})
	}
}
