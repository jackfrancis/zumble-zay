// Package httpretry wraps an http.Client so transient connectivity failures — a
// flaky cluster DNS resolver, a provider hiccup — are retried instead of failing
// the call outright. It is shared by the agent runtime (its GitHub/ZZ/model
// calls) and the web tier's OAuth client (the login token exchange), which would
// otherwise turn a one-off "server misbehaving" DNS blip into a hard failure.
//
// The retry scope is deliberately conservative to avoid duplicating a write: a
// connection-phase failure (DNS or dial — the request never reached the server)
// is retried for any method, while a mid-flight error or a 502/503/504 is retried
// only for an idempotent method.
package httpretry

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"time"
)

// Default retry policy: a few attempts with jittered exponential backoff.
const (
	DefaultAttempts    = 3
	DefaultBaseBackoff = 200 * time.Millisecond
	DefaultMaxBackoff  = 2 * time.Second
)

// Wrap returns a copy of c whose transport retries transient connectivity
// failures with the default policy. A nil client gets a fresh one; an
// already-wrapped client is returned unchanged, so calling it twice is safe.
func Wrap(c *http.Client) *http.Client {
	return WrapN(c, DefaultAttempts, DefaultBaseBackoff, DefaultMaxBackoff)
}

// WrapN is Wrap with explicit knobs (tests use a tiny backoff). attempts < 1
// collapses to a single try.
func WrapN(c *http.Client, attempts int, base, max time.Duration) *http.Client {
	if c == nil {
		c = &http.Client{}
	}
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	if _, ok := rt.(*transport); ok {
		return c
	}
	clone := *c // preserve Timeout, CheckRedirect, Jar; only swap the transport
	clone.Transport = &transport{base: rt, attempts: attempts, baseBackoff: base, maxBackoff: max}
	return &clone
}

// transport wraps a base RoundTripper with the bounded retry policy.
type transport struct {
	base        http.RoundTripper
	attempts    int
	baseBackoff time.Duration
	maxBackoff  time.Duration
}

// RoundTrip sends req, retrying transient failures up to attempts times. The
// whole loop runs inside one Client.Do call, so the client's Timeout (and the
// request context deadline) bound all attempts and backoffs together.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempts := t.attempts
	if attempts < 1 {
		attempts = 1
	}
	var (
		resp *http.Response
		err  error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Rewind the body for a repeat send; if it cannot be rewound, return the
			// last result rather than send a truncated request.
			rewound, rerr := rewind(req)
			if rerr != nil {
				return resp, err
			}
			req = rewound
			if werr := backoffWait(req.Context(), t.baseBackoff, t.maxBackoff, attempt); werr != nil {
				return nil, werr
			}
		}
		resp, err = t.base.RoundTrip(req)
		if attempt == attempts-1 || !shouldRetry(req.Method, resp, err) {
			return resp, err
		}
		drain(resp) // let the connection be reused before the next attempt
	}
	return resp, err
}

// rewind returns req ready to be sent again. A bodyless request (e.g. GET) is
// returned as-is; a body request needs GetBody so the body can be re-read, else
// it is not safely retryable.
func rewind(req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("httpretry: request body is not rewindable")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
}

// backoffWait sleeps an exponentially growing, half-jittered delay before the
// given (1-based) retry attempt, returning early if the context is cancelled.
func backoffWait(ctx context.Context, base, max time.Duration, attempt int) error {
	d := base << (attempt - 1)
	if d <= 0 || d > max {
		d = max
	}
	half := d / 2
	wait := half + time.Duration(rand.Int63n(int64(half)+1)) // full jitter in [d/2, d]
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// shouldRetry decides whether to repeat a request given its result. A request
// that never reached the server (connection-phase error) is always safe to
// repeat; anything that may have been processed is repeated only for an
// idempotent method.
func shouldRetry(method string, resp *http.Response, err error) bool {
	if err != nil {
		if isConnectionError(err) {
			return true
		}
		return isIdempotent(method)
	}
	if resp != nil && isIdempotent(method) {
		switch resp.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// isConnectionError reports whether err occurred before the request could be
// delivered — a DNS failure or a dial/connect failure — so repeating it cannot
// duplicate a server-side effect. "server misbehaving" is a *net.DNSError.
func isConnectionError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}
