package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

func TestProposeParsesAxes(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"relevance\":0.9,\"impact\":0.7,\"engagement\":0.4,\"urgency\":1.5,\"confidence\":0.8,\"rationale\":\"review requested\"}"}}]}`))
	}))
	defer srv.Close()

	r := NewRanker(Config{Endpoint: srv.URL, Model: "test-model", Token: "tok", Client: srv.Client()})
	prop, err := r.Propose(context.Background(), worklist.WorkItem{
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "k/autoscaler", Number: 1, Title: "fix"},
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if prop.Relevance != 0.9 || prop.Impact != 0.7 || prop.Engagement != 0.4 {
		t.Errorf("axes = %+v", prop)
	}
	if prop.Urgency != 1 { // clamped from 1.5
		t.Errorf("urgency = %v, want 1 (clamped)", prop.Urgency)
	}
	if prop.Confidence != 0.8 || prop.Rationale != "review requested" {
		t.Errorf("confidence/rationale = %v / %q", prop.Confidence, prop.Rationale)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
	var req chatRequest
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if req.Model != "test-model" || len(req.Messages) != 2 {
		t.Errorf("request = %+v", req)
	}
}

func TestProposeErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad model", http.StatusBadRequest)
	}))
	defer srv.Close()

	r := NewRanker(Config{Endpoint: srv.URL, Token: "tok", Client: srv.Client()})
	if _, err := r.Propose(context.Background(), worklist.WorkItem{}); err == nil {
		t.Fatal("expected an error on non-200, got nil")
	}
}

func TestStripFences(t *testing.T) {
	in := "```json\n{\"relevance\":0.5}\n```"
	if got := stripFences(in); got != `{"relevance":0.5}` {
		t.Errorf("stripFences = %q", got)
	}
}

func TestDefaultsApplied(t *testing.T) {
	r := NewRanker(Config{Token: "t"})
	if r.endpoint != DefaultEndpoint || r.model != DefaultModel {
		t.Errorf("defaults not applied: endpoint=%q model=%q", r.endpoint, r.model)
	}
}
