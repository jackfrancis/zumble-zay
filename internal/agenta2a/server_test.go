package agenta2a

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
)

// env returns a getenv backed by a map, for supplying static configuration.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// taskResponse is the shape we assert on: a JSON-RPC result carrying an A2A task.
type taskResponse struct {
	Result struct {
		Kind   string `json:"kind"`
		Status struct {
			State   string `json:"state"`
			Message struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"status"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// sendTask posts a message/send with the given metadata and returns the parsed
// response plus the raw body for diagnostics.
func sendTask(t *testing.T, srv *Server, metadata map[string]any) taskResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]any{"message": map[string]any{
			"role": "user", "kind": "message", "messageId": "m1",
			"parts":    []any{map[string]any{"kind": "text", "text": "go"}},
			"metadata": metadata,
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp taskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}

func TestParamsFromTaskMergesMetadataAndEnv(t *testing.T) {
	srv := New(WithGetenv(env(map[string]string{
		agent.EnvBaseURL: "https://zz.internal",
		agent.EnvAIToken: "model-secret", // env-only, never in metadata
	})))
	p, err := srv.paramsFromTask(map[string]any{
		agent.EnvJobType:  "llm-rank",
		agent.EnvToken:    "job-token-xyz",
		agent.EnvProvider: "github",
		agent.EnvItemID:   "gh/owner/repo#42",
	})
	if err != nil {
		t.Fatalf("paramsFromTask: %v", err)
	}
	if p.JobType != "llm-rank" || p.Token != "job-token-xyz" || p.Provider != "github" || p.ItemID != "gh/owner/repo#42" {
		t.Errorf("per-task fields not from metadata: %+v", p)
	}
	if p.BaseURL != "https://zz.internal" {
		t.Errorf("BaseURL = %q, want it from env", p.BaseURL)
	}
	if p.AIToken != "model-secret" {
		t.Errorf("AIToken = %q, want it from env only", p.AIToken)
	}
}

func TestHandleRPCAcceptsJobAndRunsDetached(t *testing.T) {
	done := make(chan agent.RunParams, 1)
	var hasDeadline bool
	srv := New(
		WithGetenv(env(map[string]string{agent.EnvBaseURL: "https://zz.internal"})),
		WithRun(func(ctx context.Context, p agent.RunParams) error {
			_, hasDeadline = ctx.Deadline()
			done <- p
			return nil
		}),
	)
	resp := sendTask(t, srv, map[string]any{
		agent.EnvJobType: "llm-rank",
		agent.EnvToken:   "tok",
	})
	// Acknowledged immediately as non-terminal; the job runs in the background.
	if resp.Result.Status.State != "submitted" {
		t.Fatalf("state = %q, want submitted (err=%v)", resp.Result.Status.State, resp.Error)
	}
	select {
	case got := <-done:
		if got.JobType != "llm-rank" || got.Token != "tok" || got.BaseURL != "https://zz.internal" {
			t.Errorf("run received wrong params: %+v", got)
		}
		if !got.ReportCompletion {
			t.Error("detached run must set ReportCompletion so its outcome reaches ZZ")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run was not invoked in the background")
	}
	if !hasDeadline {
		t.Error("detached run must get its own deadline context, not the request context")
	}
}

func TestHandleRPCRejectsInvalidParams(t *testing.T) {
	// Missing job token and base URL => ParamsFromEnv fails => failed task, run
	// never invoked.
	ran := false
	srv := New(
		WithGetenv(env(map[string]string{})),
		WithRun(func(context.Context, agent.RunParams) error { ran = true; return nil }),
	)
	resp := sendTask(t, srv, map[string]any{agent.EnvJobType: "llm-rank"})
	if resp.Result.Status.State != "failed" {
		t.Fatalf("state = %q, want failed", resp.Result.Status.State)
	}
	if ran {
		t.Error("run must not be invoked when params are invalid")
	}
	if len(resp.Result.Status.Message.Parts) == 0 || !strings.Contains(resp.Result.Status.Message.Parts[0].Text, "invalid job parameters") {
		t.Errorf("want 'invalid job parameters' message, got %+v", resp.Result.Status.Message.Parts)
	}
}

func TestServesCardForReadiness(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("card status = %d", rec.Code)
	}
	var card struct {
		Name            string `json:"name"`
		ProtocolVersion string `json:"protocolVersion"`
		Skills          []struct {
			ID string `json:"id"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.Name == "" || card.ProtocolVersion != "0.3" || len(card.Skills) == 0 {
		t.Errorf("card missing required fields: %+v", card)
	}
}

// TestEndToEndRankAsyncAgainstFakeZZ drives the real agent.Run for an llm-rank
// job against a stub ZZ that returns an empty worklist, proving the full
// A2A→params→agent.Run→ZZClient path: the response is an immediate acknowledgement
// and the job completes in the background, reporting success via the callback.
func TestEndToEndRankAsyncAgainstFakeZZ(t *testing.T) {
	var (
		mu       sync.Mutex
		gotAuth  string
		gotPaths []string
	)
	completed := make(chan string, 1) // the /agent/complete body
	zz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		if a := r.Header.Get("Authorization"); a != "" {
			gotAuth = a
		}
		mu.Unlock()
		switch {
		case r.URL.Path == "/agent/complete":
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
			completed <- string(body)
		case r.URL.Path == "/agent/worklist" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer zz.Close()

	srv := New(WithGetenv(env(map[string]string{agent.EnvBaseURL: zz.URL})))
	resp := sendTask(t, srv, map[string]any{
		agent.EnvJobType: agent.JobRank,
		agent.EnvToken:   "job-token-abc",
	})
	if resp.Result.Status.State != "submitted" {
		t.Fatalf("state = %q, want submitted (err=%v)", resp.Result.Status.State, resp.Error)
	}

	select {
	case body := <-completed:
		if strings.Contains(body, "error") {
			t.Errorf("llm-rank on an empty worklist should report success, got completion %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent never reported completion")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer job-token-abc" {
		t.Errorf("agent should present the per-task job token, got %q", gotAuth)
	}
	saw := false
	for _, p := range gotPaths {
		if p == "/agent/worklist" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("agent should GET /agent/worklist, saw %v", gotPaths)
	}
}
