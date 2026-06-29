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

func TestItemActivity(t *testing.T) {
	const me = "octocat"
	body := `[
	  {"event":"commented","actor":{"login":"alice"}},
	  {"event":"commented","actor":{"login":"bob"}},
	  {"event":"commented","actor":{"login":"alice"}},
	  {"event":"cross-referenced"},
	  {"event":"cross-referenced"},
	  {"event":"review_requested","created_at":"2026-06-20T10:00:00Z","requested_reviewer":{"login":"octocat"}}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	act, err := c.ItemActivity(context.Background(), "tok", "octo/repo", 1, me)
	if err != nil {
		t.Fatalf("ItemActivity: %v", err)
	}
	if act.Participants != 2 {
		t.Errorf("participants = %d, want 2", act.Participants)
	}
	if act.InboundRefs != 2 {
		t.Errorf("inbound_refs = %d, want 2", act.InboundRefs)
	}
	if act.AwaitingMeSince.IsZero() {
		t.Errorf("awaiting should be set (requested, not reviewed)")
	}
	if act.OtherReviewers != 0 {
		t.Errorf("other_reviewers = %d, want 0 (no one has reviewed)", act.OtherReviewers)
	}
}

func TestItemActivityReviewedClearsAwaiting(t *testing.T) {
	const me = "octocat"
	body := `[
	  {"event":"review_requested","created_at":"2026-06-20T10:00:00Z","requested_reviewer":{"login":"octocat"}},
	  {"event":"reviewed","submitted_at":"2026-06-21T10:00:00Z","user":{"login":"octocat"}}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	act, err := c.ItemActivity(context.Background(), "tok", "octo/repo", 1, me)
	if err != nil {
		t.Fatalf("ItemActivity: %v", err)
	}
	if !act.AwaitingMeSince.IsZero() {
		t.Errorf("awaiting should be cleared after review, got %v", act.AwaitingMeSince)
	}
	if act.AwaitingOthersSince.IsZero() {
		t.Errorf("awaiting-others should be set: the user reviewed, so the ball is on the author")
	}
	if act.Participants != 1 {
		t.Errorf("participants = %d, want 1 (the reviewer)", act.Participants)
	}
	if act.OtherReviewers != 0 {
		t.Errorf("other_reviewers = %d, want 0 (only the user reviewed)", act.OtherReviewers)
	}
}

// TestItemActivityOthersReviewedAwaitsAuthor covers the passive-assignee case:
// the user is on the radar (e.g. bot-assigned) but never reviewed, yet another
// reviewer has requested changes — forward progress is on the author, not the
// user, so the ball is in others' court and someone else is already engaged.
func TestItemActivityOthersReviewedAwaitsAuthor(t *testing.T) {
	const me = "octocat"
	body := `[
	  {"event":"reviewed","submitted_at":"2026-06-21T10:00:00Z","state":"changes_requested","user":{"login":"reviewer-bot"}}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	act, err := c.ItemActivity(context.Background(), "tok", "octo/repo", 1, me)
	if err != nil {
		t.Fatalf("ItemActivity: %v", err)
	}
	if !act.AwaitingMeSince.IsZero() {
		t.Errorf("awaiting-me should be zero: no review was requested of the user, got %v", act.AwaitingMeSince)
	}
	if act.AwaitingOthersSince.IsZero() {
		t.Errorf("awaiting-others should be set: another reviewer requested changes, ball on author")
	}
	if act.OtherReviewers != 1 {
		t.Errorf("other_reviewers = %d, want 1", act.OtherReviewers)
	}
}
