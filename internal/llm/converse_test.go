package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// discardLogger silences converse diagnostics in tests.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeToolBox records invocations and returns a canned result.
type fakeToolBox struct {
	calls    []string
	lastArgs string
}

func (f *fakeToolBox) Definitions() []worklist.ToolDef {
	return []worklist.ToolDef{{
		Name:        "github_read_file",
		Description: "read a file",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}}
}

func (f *fakeToolBox) Invoke(_ context.Context, name string, args json.RawMessage) (string, error) {
	f.calls = append(f.calls, name)
	f.lastArgs = string(args)
	return "go.opentelemetry.io/otel/sdk v1.41.0", nil
}

func TestConverserRunsToolLoop(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			// First turn: the model asks to read a file.
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"github_read_file","arguments":"{\"path\":\"cluster-autoscaler/go.mod\",\"ref\":\"master\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		// Second turn: the model answers in prose after the tool result.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"otel is at v1.41.0 on master, so this PR is not redundant."}}]}`)
	}))
	defer srv.Close()

	box := &fakeToolBox{}
	c := NewConverser(Config{Endpoint: srv.URL, Model: "m", Token: "tok", Client: srv.Client(), Logger: discardLogger()})
	reply, err := c.Reply(context.Background(),
		worklist.WorkItem{Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "k/a", Number: 1, Title: "bump otel"}},
		"", "", nil, "is this already on master?", box)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply != "otel is at v1.41.0 on master, so this PR is not redundant." {
		t.Fatalf("reply = %q", reply)
	}
	if len(box.calls) != 1 || box.calls[0] != "github_read_file" {
		t.Fatalf("tool calls = %v", box.calls)
	}
	if !strings.Contains(box.lastArgs, "cluster-autoscaler/go.mod") {
		t.Errorf("tool args = %q", box.lastArgs)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(bodies))
	}
	// The first request advertised the tool; the second carried the tool result.
	if !strings.Contains(bodies[0], `"tools"`) || !strings.Contains(bodies[0], "github_read_file") {
		t.Errorf("first request missing tools: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"role":"tool"`) || !strings.Contains(bodies[1], "v1.41.0") {
		t.Errorf("second request missing tool result: %s", bodies[1])
	}
}

// TestConverserAggregatesMultiChoiceToolCalls covers the Copilot-serving-Claude
// shape where one assistant turn is split across several choices: a text block in
// choices[0] and each tool call in its own later choice.
func TestConverserAggregatesMultiChoiceToolCalls(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			_, _ = io.WriteString(w, `{"choices":[`+
				`{"finish_reason":"tool_calls","message":{"role":"assistant","content":"I'll verify on the release branch."}},`+
				`{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"github_get_pull_request","arguments":"{\"number\":9518}"}}]}},`+
				`{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"toolu_2","type":"function","function":{"name":"github_read_file","arguments":"{\"path\":\"cluster-autoscaler/go.mod\"}"}}]}}`+
				`]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Not redundant: otel is still v1.41.0 on the release branch."}}]}`)
	}))
	defer srv.Close()

	box := &fakeToolBox{}
	c := NewConverser(Config{Endpoint: srv.URL, Model: "m", Token: "tok", Client: srv.Client(), Logger: discardLogger()})
	reply, err := c.Reply(context.Background(),
		worklist.WorkItem{Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "kubernetes/autoscaler", Number: 9518}},
		"", "", nil, "is this already merged?", box)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply != "Not redundant: otel is still v1.41.0 on the release branch." {
		t.Fatalf("reply = %q", reply)
	}
	// Both tool calls (from choices[1] and choices[2]) must have executed, in order.
	if len(box.calls) != 2 || box.calls[0] != "github_get_pull_request" || box.calls[1] != "github_read_file" {
		t.Fatalf("expected both tool calls executed in order, got %v", box.calls)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(bodies))
	}
	// The follow-up request carries both tool results, keyed by the Anthropic ids.
	if !strings.Contains(bodies[1], "toolu_1") || !strings.Contains(bodies[1], "toolu_2") {
		t.Errorf("second request missing tool results: %s", bodies[1])
	}
}

func TestConverserParsesLegacyFunctionCall(t *testing.T) {
	var bodies int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies++
		w.Header().Set("Content-Type", "application/json")
		if bodies == 1 {
			// Legacy single-function shape: function_call instead of tool_calls.
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"I'll check","function_call":{"name":"github_read_file","arguments":"{\"path\":\"go.mod\"}"}},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"checked: v1.41.0."}}]}`)
	}))
	defer srv.Close()

	box := &fakeToolBox{}
	c := NewConverser(Config{Endpoint: srv.URL, Model: "m", Token: "tok", Client: srv.Client(), Logger: discardLogger()})
	reply, err := c.Reply(context.Background(), worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "k/a", Number: 1}}, "", "", nil, "is it bumped?", box)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply != "checked: v1.41.0." {
		t.Fatalf("reply = %q", reply)
	}
	if len(box.calls) != 1 || box.calls[0] != "github_read_file" {
		t.Fatalf("legacy function_call not executed; calls = %v", box.calls)
	}
}

func TestConverserNoToolsSingleCall(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"tools"`) {
			t.Errorf("did not expect tools in request: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"here is a draft review request."}}]}`)
	}))
	defer srv.Close()

	c := NewConverser(Config{Endpoint: srv.URL, Model: "m", Token: "tok", Client: srv.Client(), Logger: discardLogger()})
	reply, err := c.Reply(context.Background(), worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "k/a", Number: 1}}, "", "", nil, "draft a review request", nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply != "here is a draft review request." {
		t.Fatalf("reply = %q", reply)
	}
	if calls != 1 {
		t.Fatalf("expected a single model call with no tools, got %d", calls)
	}
}

// TestReplyInjectsViewerIdentity proves the assistant is told who it is talking
// to, so it never refers the user to their own GitHub account — the "confirm
// with jackfrancis" bug observed when the user IS jackfrancis (docs/adr/0019).
func TestReplyInjectsViewerIdentity(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewConverser(Config{Endpoint: srv.URL, Model: "m", Token: "tok", Client: srv.Client(), Logger: discardLogger()})
	_, err := c.Reply(context.Background(),
		worklist.WorkItem{Type: worklist.TypePullRequest, GitHub: worklist.GitHubRef{Repo: "kubernetes/autoscaler", Number: 9411}},
		"jackfrancis", "", nil, "should I close this?", nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if !strings.Contains(body, "jackfrancis") {
		t.Fatalf("system prompt should name the viewer, got:\n%s", body)
	}
	// The guidance must frame the login as the user themselves, not a third party.
	for _, want := range []string{"THEMSELVES", "Never tell them"} {
		if !strings.Contains(body, want) {
			t.Errorf("system prompt missing viewer-identity guidance %q, got:\n%s", want, body)
		}
	}
}

// TestReplyOmitsViewerIdentityWhenUnknown confirms an empty login leaves the
// prompt unchanged, so the in-process path (no credential) is unaffected.
func TestReplyOmitsViewerIdentityWhenUnknown(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewConverser(Config{Endpoint: srv.URL, Model: "m", Token: "tok", Client: srv.Client(), Logger: discardLogger()})
	if _, err := c.Reply(context.Background(),
		worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "k/a", Number: 1}},
		"", "", nil, "what is this?", nil); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if strings.Contains(body, "THEMSELVES") {
		t.Fatalf("prompt should omit viewer-identity guidance when login is unknown, got:\n%s", body)
	}
}
