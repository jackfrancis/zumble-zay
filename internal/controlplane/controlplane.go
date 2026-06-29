// Package controlplane is the boundary between the internet-facing web tier and
// the orchestrator control plane (docs/adr/0023). The web tier triggers agent
// work (backfill a worklist, answer a conversation turn, re-rank an item) and
// asks whether a pass is still active; the orchestrator executes that work with
// its spawn privilege. Extracting the orchestrator into its own Deployment keeps
// Pod/Job-creation RBAC off the web tier, so these calls cross a process
// boundary.
//
// The same small surface is served two ways behind one Client interface: Local
// calls a co-located orchestrator directly (the single-process default, fast for
// tests and local dev), and HTTP calls the orchestrator's control API over the
// network (the split deployment). Local is deliberately deletable once the HTTP
// path is the only one in use.
package controlplane

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the web tier's view of the control plane. Every method is safe to
// call from a request path: the triggers enqueue and return; Active is a quick
// status read. All methods take a context and may fail, because in the split
// deployment they are network calls.
type Client interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
	Converse(ctx context.Context, ownerID, itemID string) error
	Research(ctx context.Context, ownerID, itemID string) error
	Active(ctx context.Context, ownerID string) (bool, error)
	// Complete forwards a runtime's terminal completion for a job (docs/adr/0024).
	Complete(ctx context.Context, jobID, errMsg string) error
}

// Controller is the orchestrator-side surface the Local adapter and the HTTP
// Handler drive. *orchestrator.Orchestrator satisfies it; defining it here keeps
// this package from importing the orchestrator (and its launcher dependencies).
type Controller interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
	Converse(ctx context.Context, ownerID, itemID string) error
	Research(ctx context.Context, ownerID, itemID string) error
	Active(ownerID string) bool
	// CompleteJob delivers a runtime's terminal completion for a job
	// (docs/adr/0024); an empty errMsg is success.
	CompleteJob(jobID, errMsg string)
}

// Local is the co-located control client: it calls an in-process orchestrator
// directly. It is the single-process default (docs/adr/0023) and the seam tests
// and local dev use to avoid standing up a second process.
type Local struct{ c Controller }

// NewLocal wraps a co-located Controller as a Client.
func NewLocal(c Controller) *Local { return &Local{c: c} }

// EnsureBackfill triggers a worklist backfill for ownerID.
func (l *Local) EnsureBackfill(ctx context.Context, ownerID string) error {
	return l.c.EnsureBackfill(ctx, ownerID)
}

// Converse schedules one assistive-conversation turn for an item.
func (l *Local) Converse(ctx context.Context, ownerID, itemID string) error {
	return l.c.Converse(ctx, ownerID, itemID)
}

// Research schedules a per-item research re-rank.
func (l *Local) Research(ctx context.Context, ownerID, itemID string) error {
	return l.c.Research(ctx, ownerID, itemID)
}

// Active reports whether a pipeline pass is still in flight for ownerID. The
// in-process call cannot fail, so the error is always nil.
func (l *Local) Active(_ context.Context, ownerID string) (bool, error) {
	return l.c.Active(ownerID), nil
}

// Complete forwards a runtime's terminal completion for a job to the co-located
// orchestrator (docs/adr/0024).
func (l *Local) Complete(_ context.Context, jobID, errMsg string) error {
	l.c.CompleteJob(jobID, errMsg)
	return nil
}

// HTTP is the remote control client: it calls the orchestrator's control API. It
// presents the shared control token as a bearer on every request, because the
// API triggers privileged spawns even though it is cluster-internal.
type HTTP struct {
	baseURL string
	client  *http.Client
	token   string
}

// NewHTTP builds a remote control client targeting the orchestrator at baseURL.
// A nil client gets http.DefaultClient.
func NewHTTP(baseURL string, client *http.Client, token []byte) *HTTP {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTP{baseURL: strings.TrimRight(baseURL, "/"), client: client, token: string(token)}
}

// EnsureBackfill triggers a worklist backfill for ownerID.
func (h *HTTP) EnsureBackfill(ctx context.Context, ownerID string) error {
	return h.trigger(ctx, "/control/ingest", request{Owner: ownerID})
}

// Converse schedules one assistive-conversation turn for an item.
func (h *HTTP) Converse(ctx context.Context, ownerID, itemID string) error {
	return h.trigger(ctx, "/control/converse", request{Owner: ownerID, Item: itemID})
}

// Research schedules a per-item research re-rank.
func (h *HTTP) Research(ctx context.Context, ownerID, itemID string) error {
	return h.trigger(ctx, "/control/research", request{Owner: ownerID, Item: itemID})
}

// Complete forwards a runtime's terminal completion for a job to the
// orchestrator's control API (docs/adr/0024).
func (h *HTTP) Complete(ctx context.Context, jobID, errMsg string) error {
	return h.trigger(ctx, "/control/complete", completeRequest{JobID: jobID, Error: errMsg})
}

// Active reports whether a pipeline pass is still in flight for ownerID.
func (h *HTTP) Active(ctx context.Context, ownerID string) (bool, error) {
	resp, err := h.do(ctx, http.MethodGet, "/control/active?owner="+url.QueryEscape(ownerID), nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("controlplane: GET /control/active: status %d", resp.StatusCode)
	}
	var out activeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("controlplane: decode active: %w", err)
	}
	return out.Active, nil
}

func (h *HTTP) trigger(ctx context.Context, path string, body any) error {
	resp, err := h.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("controlplane: POST %s: status %d", path, resp.StatusCode)
	}
	return nil
}

func (h *HTTP) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return h.client.Do(req)
}

// Handler serves the orchestrator's control API. It authenticates every request
// with a constant-time bearer check against the shared control token, then maps
// it to the wrapped Controller. It registers its routes on a caller-supplied mux
// so the orchestrator binary can add its own health endpoint alongside.
type Handler struct {
	c      Controller
	issuer TokenIssuer
	caller CallerAuthenticator
	token  []byte
	log    *slog.Logger
}

// NewHandler builds the control API handler over c, authenticated by token.
func NewHandler(c Controller, token []byte, log *slog.Logger) *Handler {
	return &Handler{c: c, token: token, log: log}
}

// WithTokenExchange enables the token-exchange endpoint (docs/adr/0024): a
// long-lived service runtime authenticates with caller and exchanges its
// identity for a fresh job-scoped token from issuer. The endpoint is registered
// only when both are set. Returns the handler for chaining.
func (h *Handler) WithTokenExchange(issuer TokenIssuer, caller CallerAuthenticator) *Handler {
	h.issuer = issuer
	h.caller = caller
	return h
}

// Register mounts the control routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /control/ingest", h.auth(h.ingest))
	mux.HandleFunc("POST /control/converse", h.auth(h.converse))
	mux.HandleFunc("POST /control/research", h.auth(h.research))
	mux.HandleFunc("POST /control/complete", h.auth(h.complete))
	mux.HandleFunc("GET /control/active", h.auth(h.active))
	if h.issuer != nil && h.caller != nil {
		// Token exchange authenticates the caller itself (RFC 8693-flavored), so it
		// is not wrapped in the shared-bearer auth the trigger routes use.
		mux.HandleFunc("POST /control/token", h.exchange)
	}
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	if req.Owner == "" {
		http.Error(w, "owner required", http.StatusBadRequest)
		return
	}
	if err := h.c.EnsureBackfill(r.Context(), req.Owner); err != nil {
		h.fail(w, "ingest", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) converse(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	if req.Owner == "" || req.Item == "" {
		http.Error(w, "owner and item required", http.StatusBadRequest)
		return
	}
	if err := h.c.Converse(r.Context(), req.Owner, req.Item); err != nil {
		h.fail(w, "converse", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) research(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	if req.Owner == "" || req.Item == "" {
		http.Error(w, "owner and item required", http.StatusBadRequest)
		return
	}
	if err := h.c.Research(r.Context(), req.Owner, req.Item); err != nil {
		h.fail(w, "research", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) active(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("owner")
	if owner == "" {
		http.Error(w, "owner required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(activeResponse{Active: h.c.Active(owner)})
}

// complete forwards a runtime's terminal completion to the orchestrator so it
// can finalize the job the instant the runtime reports, rather than waiting on
// the substrate watch (docs/adr/0024). The web tier calls this after a runtime
// posts to /agent/complete; an unknown job id is a harmless no-op downstream.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	var req completeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.JobID == "" {
		http.Error(w, "job_id required", http.StatusBadRequest)
		return
	}
	h.c.CompleteJob(req.JobID, req.Error)
	w.WriteHeader(http.StatusAccepted)
}

// exchange is the RFC 8693-flavored token-exchange endpoint (docs/adr/0024). A
// service runtime authenticates (via the CallerAuthenticator) and exchanges its
// identity for a fresh job-scoped token, the pull complement to push-at-dispatch.
func (h *Handler) exchange(w http.ResponseWriter, r *http.Request) {
	caller, err := h.caller.Authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// A trusted control-plane caller may request a token for any acting user, as
	// the orchestrator's own dispatch path does. A future per-service
	// authenticator would set Trusted false and carry a constrained authority.
	if !caller.Trusted {
		http.Error(w, "caller not permitted to request job tokens", http.StatusForbidden)
		return
	}
	var req tokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.JobType == "" || req.ActingUser == "" {
		http.Error(w, "job_type and acting_user required", http.StatusBadRequest)
		return
	}
	tok, ttl, err := h.issuer.MintJobToken(req.JobType, req.ActingUser)
	if err != nil {
		if h.log != nil {
			h.log.Warn("token exchange rejected", "caller", caller.Subject, "err", err)
		}
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(tokenResponse{
		AccessToken:     tok,
		IssuedTokenType: "urn:ietf:params:oauth:token-type:access_token",
		TokenType:       "Bearer",
		ExpiresIn:       int(ttl.Seconds()),
	})
}

func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	if h.log != nil {
		h.log.Error("control request failed", "op", op, "err", err)
	}
	http.Error(w, "control request failed", http.StatusInternalServerError)
}

// auth wraps a handler with a constant-time bearer check. An empty configured
// token rejects everything (fail closed): the control API must never run open.
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := bearer(r)
		if len(h.token) == 0 || subtle.ConstantTimeCompare([]byte(presented), h.token) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}

func decode(w http.ResponseWriter, r *http.Request) (request, bool) {
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return request{}, false
	}
	return req, true
}

type request struct {
	Owner string `json:"owner"`
	Item  string `json:"item,omitempty"`
}

type completeRequest struct {
	JobID string `json:"job_id"`
	Error string `json:"error,omitempty"`
}

type activeResponse struct {
	Active bool `json:"active"`
}

// TokenIssuer mints a job-scoped token for an authenticated caller. The
// orchestrator satisfies it via MintJobToken (docs/adr/0024).
type TokenIssuer interface {
	MintJobToken(jobType, actingUser string) (token string, expiresIn time.Duration, err error)
}

// Caller is the authenticated identity of a token-exchange client.
type Caller struct {
	// Subject identifies the caller (a service-runtime identity, or
	// "control-plane" for the default shared-bearer authenticator).
	Subject string
	// Trusted means the caller may request a token for any acting user, as the
	// orchestrator's own dispatch path does. A per-service authenticator would
	// set this false and carry a constrained authority instead.
	Trusted bool
}

// CallerAuthenticator authenticates a token-exchange request (docs/adr/0024).
// The default validates the shared control-plane bearer; a production deployment
// swaps in one that validates the caller's own platform identity (e.g. a
// projected ServiceAccount OIDC token carried in the request's subject_token).
type CallerAuthenticator interface {
	Authenticate(r *http.Request) (Caller, error)
}

// NewBearerCallerAuthenticator authenticates a caller by the shared control-plane
// bearer — the same trust boundary as the trigger routes. It is the default
// until a per-service identity authenticator is wired.
// TODO(team): validate a platform OIDC subject_token instead, so each service
// runtime carries its own constrained identity rather than a shared secret.
func NewBearerCallerAuthenticator(token []byte) CallerAuthenticator {
	return bearerCaller{token: token}
}

type bearerCaller struct{ token []byte }

func (a bearerCaller) Authenticate(r *http.Request) (Caller, error) {
	presented := bearer(r)
	if len(a.token) == 0 || subtle.ConstantTimeCompare([]byte(presented), a.token) != 1 {
		return Caller{}, errUnauthorizedCaller
	}
	return Caller{Subject: "control-plane", Trusted: true}, nil
}

var errUnauthorizedCaller = errors.New("controlplane: unauthorized caller")

type tokenRequest struct {
	JobType    string `json:"job_type"`
	ActingUser string `json:"acting_user"`
	// SubjectToken is the caller's own identity token (RFC 8693). The default
	// bearer authenticator ignores it; a platform-OIDC authenticator validates
	// it. TODO(team): wire real subject-token validation.
	SubjectToken string `json:"subject_token,omitempty"`
}

type tokenResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int    `json:"expires_in"`
}

var (
	_ Client = (*Local)(nil)
	_ Client = (*HTTP)(nil)
)
