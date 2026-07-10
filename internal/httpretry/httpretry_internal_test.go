package httpretry

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// internalStub adapts a func to http.RoundTripper for the internal tests.
type internalStub func(*http.Request) (*http.Response, error)

func (f internalStub) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A sustained 429 is retried on its own, more patient budget — independent of
// the (smaller) connection/5xx transient budget — so a rate limit is ridden out
// rather than failing after a couple of quick tries, while a connection blip
// still fails fast. Mirrors the model-provider rate limit the "review all PRs"
// fan-out hits versus a flaky-DNS blip.
func TestRateLimitAndTransientHaveSeparateBudgets(t *testing.T) {
	newTransport := func(base http.RoundTripper) *transport {
		return &transport{
			base:        base,
			attempts:    1, // no transient retry
			baseBackoff: time.Millisecond,
			maxBackoff:  time.Millisecond,
			rlAttempts:  4, // a patient rate-limit budget
			rlBase:      time.Millisecond,
			rlMax:       2 * time.Millisecond,
		}
	}

	// A sustained 429 uses the rate-limit budget → 4 tries, not 1.
	var rlCalls int
	tr := newTransport(internalStub(func(*http.Request) (*http.Response, error) {
		rlCalls++
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: http.NoBody, Header: make(http.Header)}, nil
	}))
	req, _ := http.NewRequest(http.MethodPost, "http://api.githubcopilot.com/chat/completions", strings.NewReader("{}"))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if rlCalls != 4 {
		t.Fatalf("429 attempts = %d, want 4 (rate-limit budget)", rlCalls)
	}

	// A connection error on the SAME policy uses the transient budget (1) → no retry.
	var connCalls int
	tr2 := newTransport(internalStub(func(*http.Request) (*http.Response, error) {
		connCalls++
		return nil, &net.DNSError{Err: "server misbehaving"}
	}))
	req2, _ := http.NewRequest(http.MethodPost, "http://api.github.com/x", strings.NewReader("{}"))
	if _, err := tr2.RoundTrip(req2); err == nil {
		t.Fatal("expected the connection error to surface")
	}
	if connCalls != 1 {
		t.Fatalf("connection-error attempts = %d, want 1 (transient budget)", connCalls)
	}
}

// retryAfter parses the header as seconds or an HTTP-date, and reports absence.
func TestRetryAfterParsing(t *testing.T) {
	mk := func(v string) *http.Response {
		h := make(http.Header)
		if v != "" {
			h.Set("Retry-After", v)
		}
		return &http.Response{Header: h}
	}

	if d, ok := retryAfter(mk("5")); !ok || d != 5*time.Second {
		t.Fatalf("seconds: d=%v ok=%v, want 5s true", d, ok)
	}
	if _, ok := retryAfter(mk("")); ok {
		t.Fatal("absent header should not parse")
	}
	if _, ok := retryAfter(mk("soon")); ok {
		t.Fatal("garbage header should not parse")
	}
	if _, ok := retryAfter(nil); ok {
		t.Fatal("nil response should not parse")
	}
	// A past HTTP-date clamps to zero delay (retry now), still present.
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if d, ok := retryAfter(mk(past)); !ok || d != 0 {
		t.Fatalf("past date: d=%v ok=%v, want 0 true", d, ok)
	}
}
