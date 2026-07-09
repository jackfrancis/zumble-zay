package substrate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

// capture records what the fake router received, so tests can assert the routing
// Host, the path, the metadata, and that the send was non-blocking.
type capture struct {
	host     string
	path     string
	prompt   string
	metadata map[string]any
	blocking bool
}

// newFakeRouter stands in for the atenet-router in front of the actor: it records
// the request (including the Host header the router routes on) and returns a task
// in the configured state.
func newFakeRouter(t *testing.T, state, message string, cap *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.host = r.Host
		cap.path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct {
				Message struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
					Metadata map[string]any `json:"metadata"`
				} `json:"message"`
				Configuration struct {
					Blocking bool `json:"blocking"`
				} `json:"configuration"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		cap.metadata = req.Params.Message.Metadata
		cap.blocking = req.Params.Configuration.Blocking
		if len(req.Params.Message.Parts) > 0 {
			cap.prompt = req.Params.Message.Parts[0].Text
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": "1",
			"result": map[string]any{
				"kind": "task", "id": "task-abc",
				"status": map[string]any{
					"state":   state,
					"message": map[string]any{"parts": []any{map[string]any{"kind": "text", "text": message}}},
				},
			},
		})
	}))
}

const testActorHost = "zz-runtime.zumble-zay.actors.resources.substrate.ate.dev"

func newTestLauncher(url string) *Launcher {
	return &Launcher{client: newClient(url, http.DefaultClient), actorHost: testActorHost}
}

func TestDispatchRoutesByHostSendsMetadataNonBlockingAndAccepts(t *testing.T) {
	cap := &capture{}
	srv := newFakeRouter(t, "submitted", "job llm-rank accepted", cap)
	defer srv.Close()

	handle, err := newTestLauncher(srv.URL).Dispatch(context.Background(), orchestrator.JobSpec{
		JobID: "j1", Type: orchestrator.JobLLMRank, Provider: "github", ItemID: "gh/o/r#7",
	}, "ticket-xyz")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if handle.Kind != handleKind || handle.Ref != "task-abc" {
		t.Errorf("handle = %+v, want kind=substrate ref=task-abc", handle)
	}
	// The router routes on the Host header, not the path: the actor is addressed by
	// Host and the request goes to the router root (docs/adr/0035).
	if cap.host != testActorHost {
		t.Errorf("host = %q, want %q", cap.host, testActorHost)
	}
	if cap.path != "/" {
		t.Errorf("path = %q, want /", cap.path)
	}
	if cap.blocking {
		t.Error("send must be non-blocking so the actor acknowledges immediately")
	}
	if cap.metadata[agent.EnvJobType] != "llm-rank" || cap.metadata[agent.EnvTicket] != "ticket-xyz" ||
		cap.metadata[agent.EnvProvider] != "github" || cap.metadata[agent.EnvItemID] != "gh/o/r#7" {
		t.Errorf("metadata = %+v", cap.metadata)
	}
	// The live token must never ride the metadata: the pull-path carries a
	// single-use ticket instead, so it never lands in a snapshot of the actor's
	// RAM (docs/adr/0030, 0035).
	if _, ok := cap.metadata[agent.EnvToken]; ok {
		t.Error("metadata must carry the ticket, not the job token")
	}
	// Static config must never ride the metadata: an empty ZZ_BASE_URL would
	// shadow the actor's configured value and fail its param validation.
	if _, ok := cap.metadata[agent.EnvBaseURL]; ok {
		t.Error("metadata must not carry ZZ_BASE_URL")
	}
}

func TestDispatchOmitsEmptyProviderAndItem(t *testing.T) {
	cap := &capture{}
	srv := newFakeRouter(t, "submitted", "", cap)
	defer srv.Close()

	// llm-rank has no provider and no item; those keys must be absent, not empty.
	_, err := newTestLauncher(srv.URL).Dispatch(context.Background(),
		orchestrator.JobSpec{Type: orchestrator.JobLLMRank}, "tok")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, ok := cap.metadata[agent.EnvProvider]; ok {
		t.Error("empty provider must be omitted from metadata")
	}
	if _, ok := cap.metadata[agent.EnvItemID]; ok {
		t.Error("empty item id must be omitted from metadata")
	}
}

func TestDispatchRejectsFailedTask(t *testing.T) {
	cap := &capture{}
	srv := newFakeRouter(t, "failed", "invalid job parameters: missing token", cap)
	defer srv.Close()

	handle, err := newTestLauncher(srv.URL).Dispatch(context.Background(),
		orchestrator.JobSpec{Type: orchestrator.JobLLMRank}, "tok")
	if err == nil || !strings.Contains(err.Error(), "missing token") {
		t.Fatalf("want failed-state rejection carrying the message, got %v", err)
	}
	if handle.Ref != "task-abc" {
		t.Errorf("handle should still carry the task ref on rejection, got %+v", handle)
	}
}

func TestDispatchReturnsErrorOnRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": "1",
			"error": map[string]any{"code": -32601, "message": "method not supported"},
		})
	}))
	defer srv.Close()

	_, err := newTestLauncher(srv.URL).Dispatch(context.Background(),
		orchestrator.JobSpec{Type: orchestrator.JobLLMRank}, "tok")
	if err == nil || !strings.Contains(err.Error(), "method not supported") {
		t.Fatalf("want RPC error surfaced, got %v", err)
	}
}

// TestAwaitBacksStopWithDeadline confirms Await blocks until the per-job deadline;
// the real completion arrives via the orchestrator's callback race, not here.
func TestAwaitBacksStopWithDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := newTestLauncher("http://unused").Await(ctx, orchestrator.Handle{Kind: handleKind, Ref: "task-abc"})
	if err != context.DeadlineExceeded {
		t.Fatalf("Await = %v, want context.DeadlineExceeded", err)
	}
}

func TestBuildDefaultsComposeActorHostAndEnvOverride(t *testing.T) {
	l, err := build(nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	def := l.(*Launcher)
	if def.client.routerURL != defaultRouterURL {
		t.Errorf("router = %q, want %q", def.client.routerURL, defaultRouterURL)
	}
	if def.actorHost != testActorHost {
		t.Errorf("actorHost = %q, want composed %q", def.actorHost, testActorHost)
	}

	// The atespace/actor/suffix parts compose the routing host.
	t.Setenv("SUBSTRATE_ROUTER_URL", "http://router:8000/")
	t.Setenv("SUBSTRATE_ATESPACE", "ns2")
	t.Setenv("SUBSTRATE_ACTOR", "myactor")
	t.Setenv("SUBSTRATE_DNS_SUFFIX", "actors.example.test")
	l2, err := build(nil, nil)
	if err != nil {
		t.Fatalf("build override: %v", err)
	}
	ov := l2.(*Launcher)
	if ov.client.routerURL != "http://router:8000" {
		t.Errorf("router override = %q", ov.client.routerURL)
	}
	if ov.actorHost != "myactor.ns2.actors.example.test" {
		t.Errorf("actorHost override = %q", ov.actorHost)
	}
}

func TestBuildActorHostOverrideWins(t *testing.T) {
	// A full SUBSTRATE_ACTOR_HOST wins over the composed parts (the DNS zone is
	// alpha and may drift, docs/adr/0035).
	t.Setenv("SUBSTRATE_ATESPACE", "ignored")
	t.Setenv("SUBSTRATE_ACTOR", "ignored")
	t.Setenv("SUBSTRATE_ACTOR_HOST", "explicit.host.internal")
	l, err := build(nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := l.(*Launcher).actorHost; got != "explicit.host.internal" {
		t.Errorf("actorHost = %q, want the explicit override", got)
	}
}

func TestPullsTokenIsTrue(t *testing.T) {
	// substrate is a pull substrate: the orchestrator hands it a single-use ticket,
	// not the job token, so the token never lands in a snapshot of the actor's RAM
	// (docs/adr/0030, 0035).
	if !newTestLauncher("http://unused").PullsToken() {
		t.Error("substrate launcher must report PullsToken()==true (docs/adr/0035)")
	}
}
