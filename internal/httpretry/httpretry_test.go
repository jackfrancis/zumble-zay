package httpretry_test

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/httpretry"
)

// stubRoundTripper adapts a func to http.RoundTripper for the retry tests.
type stubRoundTripper func(*http.Request) (*http.Response, error)

func (f stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResp(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: http.NoBody, Header: make(http.Header)}
}

// A transient DNS failure ("server misbehaving") on a GET is retried until it
// succeeds — the case that was failing github-enrich and the OAuth exchange.
func TestRetriesTransientDNSThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	base := stubRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) <= 2 {
			return nil, &net.DNSError{Err: "server misbehaving", Name: "api.github.com", IsTemporary: true}
		}
		return okResp(http.StatusOK), nil
	})
	c := httpretry.WrapN(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

	resp, err := c.Get("http://api.github.com/user")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (2 failures + 1 success)", got)
	}
}

// A 503 (service unavailable) means the request was not processed — an overloaded
// server, or an in-cluster gateway that could not reach its backend ("backends
// required DNS resolution which failed") — so it is retried even for a POST,
// riding out the blip instead of failing the whole job.
func TestRetriesPostOn503(t *testing.T) {
	var calls atomic.Int32
	base := stubRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) <= 1 {
			return okResp(http.StatusServiceUnavailable), nil
		}
		return okResp(http.StatusOK), nil
	})
	c := httpretry.WrapN(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

	resp, err := c.Post("http://api.githubcopilot.com/chat/completions", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a 503 retry", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (1x 503 + 1 success)", got)
	}
}

// A 504 (gateway timeout) is ambiguous — the upstream may have processed the
// request before the gateway gave up — so a non-idempotent POST is NOT retried.
func TestDoesNotRetryPostOn504(t *testing.T) {
	var calls atomic.Int32
	base := stubRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResp(http.StatusGatewayTimeout), nil
	})
	c := httpretry.WrapN(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

	resp, err := c.Post("http://zumble-zay:8080/agent/ingest", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("POST 504 retried: attempts = %d, want 1", got)
	}
}

// A connection-phase failure (DNS) on a POST IS retried, because the request
// never reached the server — and the body is rewound for the repeat send. This
// is the OAuth token exchange's case (a POST to the provider token endpoint).
func TestRetriesPostOnConnectionError(t *testing.T) {
	var calls atomic.Int32
	var gotBody atomic.Bool
	base := stubRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) <= 1 {
			return nil, &net.DNSError{Err: "server misbehaving"}
		}
		buf := make([]byte, 16)
		n, _ := r.Body.Read(buf)
		if string(buf[:n]) == `{"x":1}` {
			gotBody.Store(true)
		}
		return okResp(http.StatusAccepted), nil
	})
	c := httpretry.WrapN(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

	resp, err := c.Post("http://github.com/login/oauth/access_token", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if !gotBody.Load() {
		t.Fatal("request body was not rewound for the retry")
	}
}

// A 429 (rate limit) is retried even for a non-idempotent POST — the endpoint
// refused the request, it was not processed — so the "review all PRs" fan-out
// backs off instead of erroring the pod on a model-provider rate limit.
func TestRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	base := stubRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) <= 1 {
			return okResp(http.StatusTooManyRequests), nil
		}
		return okResp(http.StatusOK), nil
	})
	c := httpretry.WrapN(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

	resp, err := c.Post("http://api.githubcopilot.com/chat/completions", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a 429 retry", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (1x 429 + 1 success)", got)
	}
}
