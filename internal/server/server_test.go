package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// fakeConverser returns a canned reply and captures the source context it was
// handed, so a test can prove the runtime fetched live GitHub context.
type fakeConverser struct {
	gotContext chan string
}

func (f *fakeConverser) Reply(_ context.Context, _ worklist.WorkItem, sourceContext string, _ []worklist.Message, userText string, _ worklist.ToolBox) (string, error) {
	select {
	case f.gotContext <- sourceContext:
	default:
	}
	return "drafted reply to: " + userText, nil
}

// TestConverseTurnEndToEnd exercises the assistive conversation as an ephemeral
// agent (docs/adr/0019): POST /api/thread schedules a turn, the orchestrator runs
// the in-process converse runtime, which vends the user's credential, fetches the
// item's live GitHub context (a stub), asks the (faked) assistant, and writes the
// reply back; a poll of GET /api/thread then shows it.
func TestConverseTurnEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const user = "github:42"
	const ghToken = "gh-access-token-xyz"
	const itemID = "github:octo/repo#1"
	secret := []byte("test-secret-of-sufficient-length!")

	// Stub GitHub: only the Discussion endpoints the converse runtime reads. It
	// asserts the runtime presented the vended credential.
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+ghToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/1/comments"):
			_, _ = io.WriteString(w, `[{"body":"a review comment","user":{"login":"alice"}}]`)
		case strings.HasSuffix(r.URL.Path, "/issues/1"):
			_, _ = io.WriteString(w, `{"body":"the PR description"}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1/files"):
			_, _ = io.WriteString(w, `[{"filename":"main.go"}]`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer fakeGH.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	zzURL := "http://" + ln.Addr().String()

	conv := &fakeConverser{gotContext: make(chan string, 1)}
	launcher := agent.NewInProcessLauncher(zzURL, nil, log).
		WithGitHubBaseURL(fakeGH.URL).
		WithConverser(conv)

	// A non-empty AI token enables the conversation endpoint; the injected
	// converser means no model call actually happens.
	cfg := &config.Config{SessionSecret: secret, AI: config.AIConfig{Token: "enable"}}
	vlt := vault.NewMemoryVault()
	if err := vlt.Put(context.Background(), user, vault.Credential{Provider: "github", AccessToken: ghToken}); err != nil {
		t.Fatalf("seed vault: %v", err)
	}
	store := worklist.NewMemoryStore()
	store.Seed(user, worklist.WorkItem{
		ID: itemID, OwnerID: user, Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "octo/repo", Number: 1, Title: "Fix the bug"},
	})

	handler, cleanup := newWithDeps(cfg, log, launcher, vlt, store)
	defer cleanup()

	ts := httptest.NewUnstartedServer(handler)
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	defer ts.Close()

	m := mint.NewMinter(secret, time.Minute)
	bearer, err := m.Mint(mint.Claims{
		Subject: "test-trigger", ActingUserID: user,
		Scopes: []principal.Scope{principal.ScopeSignalsRead}, JobID: "trigger", Provider: "github",
	})
	if err != nil {
		t.Fatalf("mint trigger token: %v", err)
	}

	postThread(t, ts.URL, bearer, itemID, "help me triage this")

	// Poll until the assistant's reply is written back to the thread.
	var reply string
	deadline := time.After(5 * time.Second)
	for {
		msgs := getThread(t, ts.URL, bearer, itemID)
		if len(msgs) >= 2 && msgs[len(msgs)-1].Role == worklist.RoleAgent {
			reply = msgs[len(msgs)-1].Content
			break
		}
		select {
		case <-deadline:
			t.Fatalf("assistant reply never arrived; thread len=%d", len(msgs))
		case <-time.After(25 * time.Millisecond):
		}
	}
	if reply != "drafted reply to: help me triage this" {
		t.Fatalf("unexpected reply: %q", reply)
	}

	// The runtime fetched and passed live GitHub context to the assistant.
	select {
	case got := <-conv.gotContext:
		if !strings.Contains(got, "the PR description") || !strings.Contains(got, "main.go") {
			t.Fatalf("source context missing fetched GitHub data: %q", got)
		}
	default:
		t.Fatal("converser was not given any source context")
	}
}

func postThread(t *testing.T, base, bearer, id, content string) {
	t.Helper()
	body := strings.NewReader(`{"content":"` + content + `"}`)
	req, err := http.NewRequest(http.MethodPost, base+"/api/thread?id="+url.QueryEscape(id), body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("thread POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("thread POST status %d: %s", resp.StatusCode, b)
	}
}

func getThread(t *testing.T, base, bearer, id string) []worklist.Message {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/thread?id="+url.QueryEscape(id), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("thread GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("thread GET status %d: %s", resp.StatusCode, b)
	}
	var tr struct {
		Messages []worklist.Message `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode thread: %v", err)
	}
	return tr.Messages
}

// fakeResearcher returns a fixed research adjustment.
type fakeResearcher struct{ adj worklist.ResearchAdjustment }

func (f fakeResearcher) Research(_ context.Context, _ worklist.WorkItem) (worklist.ResearchAdjustment, error) {
	return f.adj, nil
}

// TestResearchAgentReweightsItem drives the github-research runtime directly
// against a real ZZ server: it reads the seeded item (foundation + thread),
// posts the (faked) research multipliers via POST /agent/research, and ZZ
// re-scores so the multipliers move the final axes (docs/adr/0022).
func TestResearchAgentReweightsItem(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const user = "github:42"
	const itemID = "github:octo/repo#1"
	secret := []byte("test-secret-of-sufficient-length!")

	cfg := &config.Config{SessionSecret: secret}
	vlt := vault.NewMemoryVault()
	store := worklist.NewMemoryStore()
	store.Seed(user, worklist.WorkItem{
		ID: itemID, OwnerID: user, Source: "github", Type: worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "octo/repo", Number: 1, Title: "CVE backport"},
		Signals: worklist.Signals{
			Proposed: &worklist.AxisProposal{Relevance: 0.8, Impact: 0.8, Engagement: 0.5, Urgency: 0.8, Rationale: "foundation"},
		},
		Thread: []worklist.Message{
			{Role: worklist.RoleUser, Content: "is this still urgent?"},
			{Role: worklist.RoleAgent, Content: "upstream declined the backport"},
		},
	})

	handler, cleanup := newWithDeps(cfg, log, nil, vlt, store)
	defer cleanup()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	m := mint.NewMinter(secret, time.Minute)
	jobToken, err := m.Mint(mint.Claims{
		Subject: "runtime-r1", ActingUserID: user,
		Scopes: []principal.Scope{principal.ScopeSignalsRead, principal.ScopeMetadataWrite}, JobID: "r1",
	})
	if err != nil {
		t.Fatalf("mint job token: %v", err)
	}

	fake := fakeResearcher{adj: worklist.ResearchAdjustment{
		Relevance: 1, Impact: 0.9, Engagement: 1, Urgency: 0.5,
		Rationale: "upstream declined the backport", AppliedAt: time.Now().UTC(),
	}}
	if err := agent.Run(context.Background(), agent.RunParams{
		JobType: agent.JobResearch, BaseURL: ts.URL, Token: jobToken, ItemID: itemID, Researcher: fake,
	}); err != nil {
		t.Fatalf("research run: %v", err)
	}

	got := getAgentItem(t, ts.URL, jobToken, itemID)
	if got.Signals.Research == nil {
		t.Fatal("research adjustment was not stored on the item")
	}
	if !nearly(got.Meta.Urgency, 0.4) {
		t.Errorf("urgency = %v, want ~0.4 (0.8×0.5)", got.Meta.Urgency)
	}
	if !nearly(got.Meta.Impact, 0.72) {
		t.Errorf("impact = %v, want ~0.72 (0.8×0.9)", got.Meta.Impact)
	}
	if !nearly(got.Meta.Relevance, 0.8) {
		t.Errorf("relevance = %v, want ~0.8 (unchanged)", got.Meta.Relevance)
	}
	if got.Meta.Rationale != "upstream declined the backport" {
		t.Errorf("rationale = %q, want the research rationale", got.Meta.Rationale)
	}
}

func getAgentItem(t *testing.T, base, bearer, id string) worklist.WorkItem {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/agent/worklist?id="+url.QueryEscape(id), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("agent worklist GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("agent worklist GET status %d: %s", resp.StatusCode, b)
	}
	var body struct {
		Items []worklist.WorkItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode agent worklist: %v", err)
	}
	if len(body.Items) == 0 {
		t.Fatalf("item %q not found in agent worklist", id)
	}
	return body.Items[0]
}

func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
