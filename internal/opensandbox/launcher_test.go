package opensandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

func testLauncher(srv *httptest.Server, opts Options) *Launcher {
	return &Launcher{client: newClient(srv.URL, "test-key", srv.Client()), opts: opts}
}

// fakeServer simulates the OpenSandbox lifecycle + execd surface this launcher
// uses: create, get (reporting the configured state), resolve endpoint (pointing
// back at this same server so the exec lands here), the execd /command, and
// delete. It records the create body, the exec body, and the headers the exec
// carried.
type fakeServer struct {
	srv       *httptest.Server
	state     string // state returned from GET /sandboxes/{id}
	createReq createSandboxRequest
	cmdReq    runCommandRequest
	cmdAuth   string // X-EXECD-ACCESS-TOKEN seen on /command
	cmdAPIKey string // OPEN-SANDBOX-API-KEY seen on /command
	deleted   chan string
}

func newFakeServer(t *testing.T, state string) *fakeServer {
	t.Helper()
	f := &fakeServer{state: state, deleted: make(chan string, 1)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			_ = json.NewDecoder(r.Body).Decode(&f.createReq)
			writeJSON(w, http.StatusCreated, sandboxInfo{ID: "sbx-1", Status: sandboxStatus{State: "Pending"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/endpoints/44772"):
			writeJSON(w, http.StatusOK, endpointInfo{
				Endpoint: strings.TrimPrefix(f.srv.URL, "http://"),
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "etok"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/sbx-1":
			writeJSON(w, http.StatusOK, sandboxInfo{ID: "sbx-1", Status: sandboxStatus{State: f.state}})
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			f.cmdAuth = r.Header.Get("X-EXECD-ACCESS-TOKEN")
			f.cmdAPIKey = r.Header.Get(apiKeyHeader)
			_ = json.NewDecoder(r.Body).Decode(&f.cmdReq)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sbx-1":
			select {
			case f.deleted <- "sbx-1":
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// TestDispatchCreatesKeepAliveSandboxAndExecsRuntime is the model + cross-substrate
// check: Dispatch creates a keep-alive sandbox (tail entrypoint, no ZZ_* container
// env) and execs the runtime into it via execd, carrying the identical ZZ_*
// injection contract through the command env (docs/adr/0027).
func TestDispatchCreatesKeepAliveSandboxAndExecsRuntime(t *testing.T) {
	f := newFakeServer(t, "Running")
	l := testLauncher(f.srv, Options{Image: "shell-img:1", ZZBaseURL: "http://zz:8080", CPU: "500m", Memory: "512Mi"})

	h, err := l.Dispatch(context.Background(), orchestrator.JobSpec{
		JobID: "j1", Type: "github-enrich", Provider: "github", ActingUserID: "github:1494193",
	}, "tok-123")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if h.Kind != handleKind || h.Ref != "sbx-1" {
		t.Errorf("handle = %+v, want {opensandbox sbx-1}", h)
	}

	// Keep-alive sandbox: tail entrypoint, shell image, NO ZZ_* on container env.
	if got := f.createReq.Entrypoint; len(got) != 3 || got[0] != "tail" {
		t.Errorf("entrypoint = %v, want keep-alive tail", got)
	}
	if f.createReq.Image == nil || f.createReq.Image.URI != "shell-img:1" {
		t.Errorf("image = %+v, want shell-img:1", f.createReq.Image)
	}
	if len(f.createReq.Env) != 0 {
		t.Errorf("container env should be empty (ZZ_* rides the exec), got %v", f.createReq.Env)
	}
	if f.createReq.Metadata["zumble-zay.dev/acting-user"] != "github-1494193" {
		t.Errorf("acting-user metadata = %q, want github-1494193 (sanitized)", f.createReq.Metadata["zumble-zay.dev/acting-user"])
	}

	// Exec: /runtime, background, ZZ_* contract delivered through execd envs.
	if f.cmdReq.Command != runtimeCommand || !f.cmdReq.Background {
		t.Errorf("command = %q background=%v, want %q background=true", f.cmdReq.Command, f.cmdReq.Background, runtimeCommand)
	}
	if f.cmdReq.Envs[agent.EnvJobType] != "github-enrich" || f.cmdReq.Envs[agent.EnvBaseURL] != "http://zz:8080" ||
		f.cmdReq.Envs[agent.EnvToken] != "tok-123" || f.cmdReq.Envs[agent.EnvProvider] != "github" {
		t.Errorf("exec env missing/incorrect: %v", f.cmdReq.Envs)
	}
	if _, ok := f.cmdReq.Envs[agent.EnvAIToken]; ok {
		t.Errorf("ZZ_AI_TOKEN must be absent when no model token is configured")
	}
	// Auth: the execd access token is forwarded from the endpoint, and the API key
	// rides along for the server-proxy path.
	if f.cmdAuth != "etok" {
		t.Errorf("execd token header = %q, want etok (forwarded from endpoint)", f.cmdAuth)
	}
	if f.cmdAPIKey != "test-key" {
		t.Errorf("api key header = %q, want test-key", f.cmdAPIKey)
	}
}

// TestDispatchMapsDeadlineToSelfReapTimeout verifies the job deadline becomes the
// keep-alive sandbox's self-reap timeout (remaining + grace).
func TestDispatchMapsDeadlineToSelfReapTimeout(t *testing.T) {
	f := newFakeServer(t, "Running")
	l := testLauncher(f.srv, Options{Image: "img", CPU: "1", Memory: "1Gi"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := l.Dispatch(ctx, orchestrator.JobSpec{Type: "github-ingest"}, "tok"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if f.createReq.Timeout == nil {
		t.Fatalf("timeout not set")
	}
	if *f.createReq.Timeout < 400 || *f.createReq.Timeout > 440 {
		t.Errorf("timeout = %d, want ~420 (2m + 5m grace)", *f.createReq.Timeout)
	}
}

// TestDispatchForwardsModelTokenAsValue checks the ranking-model token rides as a
// plain exec env value (OpenSandbox is remote; the in-cluster Secret reference
// does not apply, docs/adr/0027).
func TestDispatchForwardsModelTokenAsValue(t *testing.T) {
	f := newFakeServer(t, "Running")
	l := testLauncher(f.srv, Options{Image: "img", AIToken: "ai-secret", CPU: "1", Memory: "1Gi"})
	if _, err := l.Dispatch(context.Background(), orchestrator.JobSpec{Type: "llm-rank"}, "tok"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if f.cmdReq.Envs[agent.EnvAIToken] != "ai-secret" {
		t.Errorf("ZZ_AI_TOKEN = %q, want ai-secret (forwarded as a value)", f.cmdReq.Envs[agent.EnvAIToken])
	}
}

// TestDispatchFailsAndCleansUpWhenSandboxNeverRuns verifies Dispatch fails fast on
// a terminal sandbox state and best-effort deletes the sandbox so none leaks.
func TestDispatchFailsAndCleansUpWhenSandboxNeverRuns(t *testing.T) {
	f := newFakeServer(t, "Failed")
	l := testLauncher(f.srv, Options{Image: "img", CPU: "1", Memory: "1Gi"})
	if _, err := l.Dispatch(context.Background(), orchestrator.JobSpec{Type: "github-ingest"}, "tok"); err == nil {
		t.Fatalf("Dispatch: want error when the sandbox enters Failed")
	}
	select {
	case <-f.deleted:
	case <-time.After(2 * time.Second):
		t.Fatalf("failed sandbox was not cleaned up")
	}
}

// TestAwaitWaitsForDeadlineThenDeletes verifies the detached Await blocks until the
// context is done (the deadline backstop, docs/adr/0025) and then best-effort
// deletes the sandbox.
func TestAwaitWaitsForDeadlineThenDeletes(t *testing.T) {
	f := newFakeServer(t, "Running")
	l := testLauncher(f.srv, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Await(ctx, orchestrator.Handle{Kind: handleKind, Ref: "sbx-1"}); err == nil {
		t.Fatalf("Await returned nil, want context deadline error")
	}
	select {
	case <-f.deleted:
	case <-time.After(2 * time.Second):
		t.Fatalf("best-effort delete was not called")
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
