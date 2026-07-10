package agent

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// retryStubRoundTripper adapts a func to http.RoundTripper for the retry tests.
type retryStubRoundTripper func(*http.Request) (*http.Response, error)

func (f retryStubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResp(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: http.NoBody, Header: make(http.Header)}
}

// A transient DNS failure ("server misbehaving") on a GET is retried until it
// succeeds — the exact case that was failing github-enrich.
func TestRetryTransportRetriesTransientDNSThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	base := retryStubRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) <= 2 {
			return nil, &net.DNSError{Err: "server misbehaving", Name: "api.github.com", IsTemporary: true}
		}
		return okResp(http.StatusOK), nil
	})
	c := wrapRetry(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

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

// A non-idempotent POST that reaches the server and gets a 503 is NOT retried:
// the request may have been processed, so repeating it could double-write.
func TestRetryTransportDoesNotRetryPostOn503(t *testing.T) {
	var calls atomic.Int32
	base := retryStubRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResp(http.StatusServiceUnavailable), nil
	})
	c := wrapRetry(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

	resp, err := c.Post("http://zumble-zay:8080/agent/ingest", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("POST 503 retried: attempts = %d, want 1", got)
	}
}

// A connection-phase failure (DNS) on a POST IS retried, because the request
// never reached the server — and the body is rewound for the repeat send.
func TestRetryTransportRetriesPostOnConnectionError(t *testing.T) {
	var calls atomic.Int32
	var gotBody atomic.Bool
	base := retryStubRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) <= 1 {
			return nil, &net.DNSError{Err: "server misbehaving"}
		}
		// On the retry the body must be re-readable (GetBody rewind).
		buf := make([]byte, 16)
		n, _ := r.Body.Read(buf)
		if string(buf[:n]) == `{"x":1}` {
			gotBody.Store(true)
		}
		return okResp(http.StatusAccepted), nil
	})
	c := wrapRetry(&http.Client{Transport: base}, 3, time.Millisecond, 2*time.Millisecond)

	resp, err := c.Post("http://zumble-zay:8080/agent/complete", "application/json", strings.NewReader(`{"x":1}`))
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
