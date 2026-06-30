package opensandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

// testLauncher wires a Launcher to a test server's client.
func testLauncher(srv *httptest.Server, opts Options) *Launcher {
	return &Launcher{client: newClient(srv.URL, "test-key", srv.Client()), opts: opts}
}

// TestDispatchCreatesSandboxWithRuntimeContract is the cross-substrate regression
// check: the create request carries the identical ZZ_* injection contract as the
// Job/Pod/Sandbox launchers, plus the runtime image and its entrypoint
// (docs/adr/0012, 0027).
func TestDispatchCreatesSandboxWithRuntimeContract(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey string
	var gotReq createSandboxRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAPIKey = r.Method, r.URL.Path, r.Header.Get(apiKeyHeader)
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sandboxInfo{ID: "sbx-123", Status: sandboxStatus{State: "Pending"}})
	}))
	defer srv.Close()

	l := testLauncher(srv, Options{Image: "img:1", ZZBaseURL: "http://zz:8080", CPU: "500m", Memory: "512Mi"})
	h, err := l.Dispatch(context.Background(), orchestrator.JobSpec{
		JobID: "j1", Type: "github-enrich", Provider: "github", ActingUserID: "github:1494193",
	}, "tok-123")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/sandboxes" {
		t.Errorf("request = %s %s, want POST /sandboxes", gotMethod, gotPath)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("api key header = %q, want test-key", gotAPIKey)
	}
	if h.Kind != handleKind || h.Ref != "sbx-123" {
		t.Errorf("handle = %+v, want {opensandbox sbx-123}", h)
	}
	if gotReq.Image == nil || gotReq.Image.URI != "img:1" {
		t.Errorf("image = %+v, want uri img:1", gotReq.Image)
	}
	if len(gotReq.Entrypoint) != 1 || gotReq.Entrypoint[0] != runtimeEntrypoint {
		t.Errorf("entrypoint = %v, want [%s]", gotReq.Entrypoint, runtimeEntrypoint)
	}
	if gotReq.ResourceLimits["cpu"] != "500m" || gotReq.ResourceLimits["memory"] != "512Mi" {
		t.Errorf("resourceLimits = %v, want cpu=500m memory=512Mi", gotReq.ResourceLimits)
	}
	if gotReq.Env[agent.EnvJobType] != "github-enrich" || gotReq.Env[agent.EnvBaseURL] != "http://zz:8080" ||
		gotReq.Env[agent.EnvToken] != "tok-123" || gotReq.Env[agent.EnvProvider] != "github" {
		t.Errorf("injection env missing/incorrect: %v", gotReq.Env)
	}
	if _, ok := gotReq.Env[agent.EnvAIToken]; ok {
		t.Errorf("ZZ_AI_TOKEN must be absent when no model token is configured")
	}
	if gotReq.Metadata["zumble-zay.dev/acting-user"] != "github-1494193" {
		t.Errorf("acting-user metadata = %q, want github-1494193 (sanitized)", gotReq.Metadata["zumble-zay.dev/acting-user"])
	}
}

// TestDispatchMapsDeadlineToSelfReapTimeout verifies the job deadline becomes the
// sandbox's self-reap timeout (remaining + grace), so a finished sandbox cleans
// itself up even if the prompt delete is missed.
func TestDispatchMapsDeadlineToSelfReapTimeout(t *testing.T) {
	var gotReq createSandboxRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sandboxInfo{ID: "sbx-1"})
	}))
	defer srv.Close()

	l := testLauncher(srv, Options{Image: "img", CPU: "1", Memory: "1Gi"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := l.Dispatch(ctx, orchestrator.JobSpec{Type: "github-ingest"}, "tok"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if gotReq.Timeout == nil {
		t.Fatalf("timeout not set")
	}
	// 2m remaining + 5m grace ≈ 420s.
	if *gotReq.Timeout < 400 || *gotReq.Timeout > 440 {
		t.Errorf("timeout = %d, want ~420 (2m + 5m grace)", *gotReq.Timeout)
	}
}

// TestDispatchForwardsModelTokenAsValue checks the ranking-model token rides as a
// plain env value (OpenSandbox is remote; the in-cluster Secret reference does not
// apply, docs/adr/0027).
func TestDispatchForwardsModelTokenAsValue(t *testing.T) {
	var gotReq createSandboxRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sandboxInfo{ID: "sbx-ai"})
	}))
	defer srv.Close()

	l := testLauncher(srv, Options{Image: "img", AIToken: "ai-secret", CPU: "1", Memory: "1Gi"})
	if _, err := l.Dispatch(context.Background(), orchestrator.JobSpec{Type: "llm-rank"}, "tok"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if gotReq.Env[agent.EnvAIToken] != "ai-secret" {
		t.Errorf("ZZ_AI_TOKEN = %q, want ai-secret (forwarded as a value)", gotReq.Env[agent.EnvAIToken])
	}
}

// TestAwaitWaitsForDeadlineThenDeletes verifies the detached Await blocks until
// the context is done (the deadline backstop, docs/adr/0025) and then best-effort
// deletes the sandbox so it does not linger until its self-reap timeout.
func TestAwaitWaitsForDeadlineThenDeletes(t *testing.T) {
	gotDelete := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			gotDelete <- r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	l := testLauncher(srv, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Await(ctx, orchestrator.Handle{Kind: handleKind, Ref: "sbx-await"}); err == nil {
		t.Fatalf("Await returned nil, want context deadline error")
	}
	select {
	case path := <-gotDelete:
		if path != "/sandboxes/sbx-await" {
			t.Errorf("delete path = %q, want /sandboxes/sbx-await", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("best-effort delete was not called")
	}
}

// TestDispatchSurfacesServerError verifies a non-2xx lifecycle response fails the
// dispatch rather than returning a bogus handle.
func TestDispatchSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid request"}`))
	}))
	defer srv.Close()

	l := testLauncher(srv, Options{Image: "img", CPU: "1", Memory: "1Gi"})
	if _, err := l.Dispatch(context.Background(), orchestrator.JobSpec{Type: "github-ingest"}, "tok"); err == nil {
		t.Fatalf("Dispatch with HTTP 400: want error")
	}
}

// TestBuildRequiresEndpointAndKey verifies build fails fast without the OpenSandbox
// endpoint and key, and otherwise returns a Launcher (docs/adr/0027).
func TestBuildRequiresEndpointAndKey(t *testing.T) {
	cfg := &config.Config{}

	t.Setenv("OPENSANDBOX_ENDPOINT", "")
	t.Setenv("OPENSANDBOX_API_KEY", "")
	if _, err := build(cfg, nil); err == nil {
		t.Fatalf("build with no endpoint/key: want error")
	}

	t.Setenv("OPENSANDBOX_ENDPOINT", "https://opensandbox:8443/v1")
	t.Setenv("OPENSANDBOX_API_KEY", "key")
	l, err := build(cfg, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := l.(*Launcher); !ok {
		t.Fatalf("build returned %T, want *Launcher", l)
	}
}
