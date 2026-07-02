package controlplane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/controlplane"
)

// fakeController records the calls the adapters and handler make.
type fakeController struct {
	mu        sync.Mutex
	ingest    []string
	converse  [][2]string
	research  [][2]string
	completed [][2]string
	active    map[string]bool
	redeemed  []string
	redeemTok string
}

func (f *fakeController) EnsureBackfill(_ context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ingest = append(f.ingest, owner)
	return nil
}

func (f *fakeController) Converse(_ context.Context, owner, item string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.converse = append(f.converse, [2]string{owner, item})
	return nil
}

func (f *fakeController) Research(_ context.Context, owner, item string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.research = append(f.research, [2]string{owner, item})
	return nil
}

func (f *fakeController) Active(owner string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[owner]
}

func (f *fakeController) CompleteJob(jobID, errMsg string) {
	f.mu.Lock()
	f.completed = append(f.completed, [2]string{jobID, errMsg})
	f.mu.Unlock()
}

func (f *fakeController) RedeemTicket(ticket string) (string, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redeemed = append(f.redeemed, ticket)
	return f.redeemTok, 10 * time.Minute, nil
}

func TestLocalDelegatesToController(t *testing.T) {
	fc := &fakeController{active: map[string]bool{"u1": true}}
	c := controlplane.NewLocal(fc)
	ctx := context.Background()

	if err := c.EnsureBackfill(ctx, "u1"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	if err := c.Converse(ctx, "u1", "i1"); err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if err := c.Research(ctx, "u1", "i2"); err != nil {
		t.Fatalf("Research: %v", err)
	}
	active, err := c.Active(ctx, "u1")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if !active {
		t.Fatal("Active should report true for u1")
	}
	if len(fc.ingest) != 1 || fc.ingest[0] != "u1" {
		t.Fatalf("ingest not delegated: %v", fc.ingest)
	}
	if len(fc.converse) != 1 || fc.converse[0] != [2]string{"u1", "i1"} {
		t.Fatalf("converse not delegated: %v", fc.converse)
	}
	if len(fc.research) != 1 || fc.research[0] != [2]string{"u1", "i2"} {
		t.Fatalf("research not delegated: %v", fc.research)
	}
}

func TestHTTPRoundTripsThroughHandler(t *testing.T) {
	const token = "control-token-abc"
	fc := &fakeController{active: map[string]bool{"u1": true}}
	h := controlplane.NewHandler(fc, []byte(token), nil)
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := controlplane.NewHTTP(ts.URL, ts.Client(), []byte(token))
	ctx := context.Background()

	if err := c.EnsureBackfill(ctx, "u1"); err != nil {
		t.Fatalf("EnsureBackfill: %v", err)
	}
	if err := c.Converse(ctx, "u1", "i1"); err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if err := c.Research(ctx, "u1", "i2"); err != nil {
		t.Fatalf("Research: %v", err)
	}
	active, err := c.Active(ctx, "u1")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if !active {
		t.Fatal("Active should round-trip true for u1")
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.ingest) != 1 || fc.ingest[0] != "u1" {
		t.Fatalf("ingest not received: %v", fc.ingest)
	}
	if len(fc.converse) != 1 || fc.converse[0] != [2]string{"u1", "i1"} {
		t.Fatalf("converse not received: %v", fc.converse)
	}
	if len(fc.research) != 1 || fc.research[0] != [2]string{"u1", "i2"} {
		t.Fatalf("research not received: %v", fc.research)
	}
}

func TestCompleteForwardsToController(t *testing.T) {
	const token = "control-token-abc"
	fc := &fakeController{active: map[string]bool{}}
	h := controlplane.NewHandler(fc, []byte(token), nil)
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := controlplane.NewHTTP(ts.URL, ts.Client(), []byte(token))
	if err := c.Complete(context.Background(), "job-1", "boom"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.completed) != 1 || fc.completed[0] != [2]string{"job-1", "boom"} {
		t.Fatalf("completion not forwarded to the controller: %v", fc.completed)
	}
}

func TestLocalRedeemTicket(t *testing.T) {
	fc := &fakeController{active: map[string]bool{}, redeemTok: "job-token"}
	c := controlplane.NewLocal(fc)
	tok, exp, err := c.RedeemTicket(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}
	if tok != "job-token" || exp != 600 {
		t.Fatalf("unexpected: tok=%q exp=%d (want job-token / 600s)", tok, exp)
	}
	if len(fc.redeemed) != 1 || fc.redeemed[0] != "t1" {
		t.Fatalf("ticket not forwarded to the controller: %v", fc.redeemed)
	}
}

func TestRedeemTicketForwardsToController(t *testing.T) {
	const token = "control-token-abc"
	fc := &fakeController{active: map[string]bool{}, redeemTok: "minted-job-token"}
	h := controlplane.NewHandler(fc, []byte(token), nil)
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := controlplane.NewHTTP(ts.URL, ts.Client(), []byte(token))
	tok, exp, err := c.RedeemTicket(context.Background(), "ticket-xyz")
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}
	if tok != "minted-job-token" || exp != 600 {
		t.Fatalf("unexpected redeem response: tok=%q exp=%d", tok, exp)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.redeemed) != 1 || fc.redeemed[0] != "ticket-xyz" {
		t.Fatalf("ticket not forwarded to the controller: %v", fc.redeemed)
	}
}

func TestHandlerRejectsBadToken(t *testing.T) {
	fc := &fakeController{}
	h := controlplane.NewHandler(fc, []byte("right-token"), nil)
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// A client with the wrong token must be rejected, and the controller must
	// never be reached.
	c := controlplane.NewHTTP(ts.URL, ts.Client(), []byte("wrong-token"))
	if err := c.EnsureBackfill(context.Background(), "u1"); err == nil {
		t.Fatal("expected an error for a bad control token")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.ingest) != 0 {
		t.Fatalf("controller was reached despite a bad token: %v", fc.ingest)
	}
}

// fakeIssuer records token-exchange requests and returns a canned token.
type fakeIssuer struct {
	mu    sync.Mutex
	calls [][2]string
}

func (f *fakeIssuer) MintJobToken(jobType, actingUser string) (string, time.Duration, error) {
	f.mu.Lock()
	f.calls = append(f.calls, [2]string{jobType, actingUser})
	f.mu.Unlock()
	return "minted-job-token", 10 * time.Minute, nil
}

func TestTokenExchangeIssuesScopedToken(t *testing.T) {
	const ctrlToken = "control-token-abc"
	fc := &fakeController{active: map[string]bool{}}
	issuer := &fakeIssuer{}
	h := controlplane.NewHandler(fc, []byte(ctrlToken), nil).
		WithTokenExchange(issuer)
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	post := func(bearer, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/control/token", strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		return resp
	}

	// An authenticated caller exchanges its identity for a job token.
	resp := post(ctrlToken, `{"job_type":"github-ingest","acting_user":"github:7"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.AccessToken != "minted-job-token" || out.TokenType != "Bearer" || out.ExpiresIn != 600 {
		t.Fatalf("unexpected token response: %+v", out)
	}
	if len(issuer.calls) != 1 || issuer.calls[0] != [2]string{"github-ingest", "github:7"} {
		t.Fatalf("issuer not called with the request params: %v", issuer.calls)
	}

	// A wrong bearer is rejected before the issuer is reached.
	resp = post("nope", `{"job_type":"github-ingest","acting_user":"github:7"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	if len(issuer.calls) != 1 {
		t.Fatalf("issuer reached despite a bad bearer: %v", issuer.calls)
	}

	// Missing required fields are a client error.
	resp = post(ctrlToken, `{"job_type":"github-ingest"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing-field status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTokenExchangeDisabledWhenUnset(t *testing.T) {
	const ctrlToken = "control-token-abc"
	h := controlplane.NewHandler(&fakeController{}, []byte(ctrlToken), nil)
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/control/token", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+ctrlToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// Without WithTokenExchange the route is not registered.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route not registered)", resp.StatusCode)
	}
}
