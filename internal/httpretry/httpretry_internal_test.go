package httpretry

import (
	"net/http"
	"testing"
	"time"
)

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
