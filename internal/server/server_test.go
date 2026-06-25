package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/agent"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/mint"
	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/vault"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// githubSearchBody is the stub /search/issues response: two open PRs in one
// repo. Returned for every query, so the agent's three signal queries dedupe to
// two unique work items.
const githubSearchBody = `{"items":[
  {"number":1,"title":"Fix the bug","html_url":"https://github.com/octo/repo/pull/1","state":"open","updated_at":"2026-06-20T10:00:00Z","repository_url":"https://api.github.com/repos/octo/repo","pull_request":{"url":"https://api.github.com/repos/octo/repo/pulls/1"}},
  {"number":2,"title":"Add the feature","html_url":"https://github.com/octo/repo/pull/2","state":"open","updated_at":"2026-06-21T10:00:00Z","repository_url":"https://api.github.com/repos/octo/repo","pull_request":{"url":"https://api.github.com/repos/octo/repo/pulls/2"}}
]}`

type worklistResp struct {
	Status string              `json:"status"`
	Items  []worklist.WorkItem `json:"items"`
}

// TestAgenticBackfillEndToEnd exercises the whole loop over real HTTP: an empty
// worklist GET returns "processing" and triggers the orchestrator, which mints a
// job token and runs the in-process agent; the agent vends the user's GitHub
// credential from ZZ, calls GitHub directly (a stub), and posts results to the
// ingest sink; a later GET then returns the populated, agent-attributed items.
func TestAgenticBackfillEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const user = "github:42"
	const ghToken = "gh-access-token-xyz"
	secret := []byte("test-secret-of-sufficient-length!")

	// Stub GitHub. It asserts the agent presented the vended credential, proving
	// the credential flowed ZZ vault -> vend -> agent -> GitHub.
	var ghCalls int32
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+ghToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/search/issues":
			atomic.AddInt32(&ghCalls, 1)
			_, _ = io.WriteString(w, githubSearchBody)
		case r.URL.Path == "/user":
			// The chained github-enrich runtime resolves "me" before scanning
			// review-request timelines.
			_, _ = io.WriteString(w, `{"login":"octo-me"}`)
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			_, _ = io.WriteString(w, `[]`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer fakeGH.Close()

	// Bind ZZ's listener up front so the in-process agent has a stable loopback
	// URL to call back into.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	zzURL := "http://" + ln.Addr().String()

	launcher := agent.NewInProcessLauncher(zzURL, nil, log).WithGitHubBaseURL(fakeGH.URL)

	cfg := &config.Config{SessionSecret: secret}
	vlt := vault.NewMemoryVault()
	if err := vlt.Put(context.Background(), user, vault.Credential{Provider: "github", AccessToken: ghToken}); err != nil {
		t.Fatalf("seed vault: %v", err)
	}
	store := worklist.NewMemoryStore()

	handler, cleanup := newWithDeps(cfg, log, launcher, vlt, store)
	defer cleanup()

	ts := httptest.NewUnstartedServer(handler)
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	defer ts.Close()

	// A workload bearer scoped to the user, used only to trigger the read path.
	m := mint.NewMinter(secret, time.Minute)
	bearer, err := m.Mint(mint.Claims{
		Subject:      "test-trigger",
		ActingUserID: user,
		Scopes:       []principal.Scope{principal.ScopeSignalsRead},
		JobID:        "trigger",
		Provider:     "github",
	})
	if err != nil {
		t.Fatalf("mint trigger token: %v", err)
	}

	// First read: empty store -> processing, and a backfill is kicked off.
	first := getWorklist(t, ts.URL, bearer)
	if first.Status != "processing" {
		t.Fatalf("expected status processing, got %q", first.Status)
	}
	if len(first.Items) != 0 {
		t.Fatalf("expected no items on first read, got %d", len(first.Items))
	}

	// Poll until the agent has populated the store.
	var ready worklistResp
	deadline := time.After(5 * time.Second)
	for {
		ready = getWorklist(t, ts.URL, bearer)
		if ready.Status == "ready" && len(ready.Items) == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("worklist never became ready: status=%q items=%d", ready.Status, len(ready.Items))
		case <-time.After(25 * time.Millisecond):
		}
	}

	for _, it := range ready.Items {
		if it.OwnerID != user {
			t.Fatalf("item owner %q != %q", it.OwnerID, user)
		}
		if it.Meta.Origin != worklist.OriginAgent {
			t.Fatalf("item origin %q != %q", it.Meta.Origin, worklist.OriginAgent)
		}
		if it.Source != "github" || it.Type != worklist.TypePullRequest {
			t.Fatalf("unexpected item shape: %+v", it)
		}
	}
	if atomic.LoadInt32(&ghCalls) == 0 {
		t.Fatal("agent never called GitHub")
	}
}

func getWorklist(t *testing.T, base, bearer string) worklistResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/worklist", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("worklist GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("worklist GET status %d: %s", resp.StatusCode, body)
	}
	var wr worklistResp
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		t.Fatalf("decode worklist: %v", err)
	}
	return wr
}
