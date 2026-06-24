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
  {"number":1,"title":"Fix bug","html_url":"https://github.com/octo/repo/pull/1","state":"open","updated_at":"2026-06-20T10:00:00Z","repository_url":"https://api.github.com/repos/octo/repo","pull_request":{"url":"https://api.github.com/repos/octo/repo/pulls/1"}},
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
