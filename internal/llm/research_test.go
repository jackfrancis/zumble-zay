package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

func TestResearchParsesMultipliersAndDefaultsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// urgency omitted entirely -> defaults to 1.0; impact explicit 0.0 -> kept.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"relevance\":1.0,\"impact\":0.0,\"engagement\":1.0,\"rationale\":\"evidence\"}"}}]}`)
	}))
	defer srv.Close()

	r := NewResearchRanker(Config{Endpoint: srv.URL, Model: "m", Token: "t", Client: srv.Client()})
	adj, err := r.Research(context.Background(), worklist.WorkItem{
		GitHub:  worklist.GitHubRef{Repo: "k/a", Number: 1, Title: "x"},
		Signals: worklist.Signals{Proposed: &worklist.AxisProposal{Relevance: 0.8, Impact: 0.8, Engagement: 0.5, Urgency: 0.8}},
		Thread:  []worklist.Message{{Role: worklist.RoleUser, Content: "is this needed?"}, {Role: worklist.RoleAgent, Content: "yes, verified"}},
	})
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if adj.Relevance != 1.0 || adj.Engagement != 1.0 {
		t.Errorf("relevance/engagement = %v/%v, want 1.0", adj.Relevance, adj.Engagement)
	}
	if adj.Impact != 0.0 {
		t.Errorf("impact = %v, want 0.0 (explicit drop honored)", adj.Impact)
	}
	if adj.Urgency != 1.0 {
		t.Errorf("urgency = %v, want 1.0 (omitted key defaults to neutral)", adj.Urgency)
	}
	if adj.Rationale != "evidence" || adj.AppliedAt.IsZero() {
		t.Errorf("rationale/appliedAt = %q/%v", adj.Rationale, adj.AppliedAt)
	}
}

func TestResearchNoThreadIsNeutralWithoutModelCall(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewResearchRanker(Config{Endpoint: srv.URL, Token: "t", Client: srv.Client()})
	adj, err := r.Research(context.Background(), worklist.WorkItem{})
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if adj.Relevance != 1 || adj.Impact != 1 || adj.Engagement != 1 || adj.Urgency != 1 {
		t.Errorf("no-thread adjustment should be neutral, got %+v", adj)
	}
	if called {
		t.Error("the model must not be called when there is no thread")
	}
}
