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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/runtimestats"
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
	// Complete forwards a runtime's terminal completion for a job (docs/adr/0024),
	// with the runtime's self-reported phase timing.
	Complete(ctx context.Context, jobID, errMsg string, timing runtimestats.Timing) error
	// RedeemTicket exchanges a pull substrate's single-use ticket for its job
	// token (docs/adr/0029); expiresIn is the token lifetime in seconds.
	RedeemTicket(ctx context.Context, ticket string) (token string, expiresIn int, err error)
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
	// (docs/adr/0024); an empty errMsg is success. timing is the runtime's
	// self-reported phase breakdown, for metrics.
	CompleteJob(jobID, errMsg string, timing runtimestats.Timing)
	// RedeemTicket exchanges a pull substrate's single-use ticket for its job
	// token (docs/adr/0029). The ticket is itself the authorization.
	RedeemTicket(ticket string) (token string, ttl time.Duration, err error)
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
func (l *Local) Complete(_ context.Context, jobID, errMsg string, timing runtimestats.Timing) error {
	l.c.CompleteJob(jobID, errMsg, timing)
	return nil
}

// RedeemTicket redeems a pull substrate's single-use ticket against the
// co-located orchestrator (docs/adr/0029).
func (l *Local) RedeemTicket(_ context.Context, ticket string) (string, int, error) {
	tok, ttl, err := l.c.RedeemTicket(ticket)
	if err != nil {
		return "", 0, err
	}
	return tok, int(ttl.Seconds()), nil
}

// HTTP is the remote control client: it calls the orchestrator's control API. It
// presents its own Kubernetes workload identity — a projected ServiceAccount
// token — as a bearer on every request, because the API triggers privileged
// spawns even though it is cluster-internal. The token is read from a source per
// request so the credential the kubelet rotates in place stays current
// (docs/adr/0031, 0034).
type HTTP struct {
	baseURL string
	client  *http.Client
	token   func() (string, error)
}

// NewHTTP builds a remote control client targeting the orchestrator at baseURL.
// source supplies the caller's bearer — a projected ServiceAccount token — on
// every request, so the kubelet-rotated credential is always presented current
// (docs/adr/0031, 0034). A nil client gets http.DefaultClient.
func NewHTTP(baseURL string, client *http.Client, source func() (string, error)) *HTTP {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTP{baseURL: strings.TrimRight(baseURL, "/"), client: client, token: source}
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
func (h *HTTP) Complete(ctx context.Context, jobID, errMsg string, timing runtimestats.Timing) error {
	return h.trigger(ctx, "/control/complete", completeRequest{JobID: jobID, Error: errMsg, Timing: timing})
}

// RedeemTicket redeems a pull substrate's single-use ticket against the
// orchestrator's control API (docs/adr/0029), returning the job token and its
// lifetime in seconds.
func (h *HTTP) RedeemTicket(ctx context.Context, ticket string) (string, int, error) {
	resp, err := h.do(ctx, http.MethodPost, "/control/redeem", ticketRequest{Ticket: ticket})
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("controlplane: POST /control/redeem: status %d", resp.StatusCode)
	}
	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("controlplane: decode redeem: %w", err)
	}
	return out.AccessToken, out.ExpiresIn, nil
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
	if h.token != nil {
		tok, err := h.token()
		if err != nil {
			return nil, fmt.Errorf("controlplane: read control token: %w", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return h.client.Do(req)
}

// Handler serves the orchestrator's control API. It authenticates every request
// through a CallerAuthenticator — the caller's per-service Kubernetes workload
// identity, a projected ServiceAccount token validated by TokenReview
// (docs/adr/0031, 0034) — then maps it to the wrapped Controller. It registers
// its routes on a caller-supplied mux so the orchestrator binary can add its own
// health endpoint alongside.
type Handler struct {
	c      Controller
	issuer TokenIssuer
	caller CallerAuthenticator
	log    *slog.Logger
}

// NewHandler builds the control API handler over c. Every route is authenticated
// by caller — the caller's per-service Kubernetes workload identity (docs/adr/0031,
// 0034). A nil caller denies every request (fail closed): the control API must
// never run open.
func NewHandler(c Controller, caller CallerAuthenticator, log *slog.Logger) *Handler {
	if caller == nil {
		caller = denyAllCaller{}
	}
	return &Handler{c: c, caller: caller, log: log}
}

// WithTokenExchange enables the token-exchange endpoint (docs/adr/0024): a caller
// exchanges its identity for a fresh job-scoped token from issuer. It is
// registered only when issuer is set and is authenticated by the handler's caller
// (WithCaller), the same as every other control route.
func (h *Handler) WithTokenExchange(issuer TokenIssuer) *Handler {
	h.issuer = issuer
	return h
}

// Register mounts the control routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /control/ingest", h.auth(h.ingest))
	mux.HandleFunc("POST /control/converse", h.auth(h.converse))
	mux.HandleFunc("POST /control/research", h.auth(h.research))
	mux.HandleFunc("POST /control/complete", h.auth(h.complete))
	mux.HandleFunc("POST /control/redeem", h.auth(h.redeem))
	mux.HandleFunc("GET /control/active", h.auth(h.active))
	if h.issuer != nil {
		mux.HandleFunc("POST /control/token", h.auth(h.exchange))
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
	h.c.CompleteJob(req.JobID, req.Error, req.Timing)
	w.WriteHeader(http.StatusAccepted)
}

// redeem exchanges a pull substrate's single-use ticket for the job token it was
// issued for (docs/adr/0029), the orchestrator half of the web tier's POST
// /agent/token. The ticket is the authorization — single-use, and the
// orchestrator issues exactly one per dispatched job — so this sits behind the
// same per-service caller identity as the other trigger routes; the web tier is
// the only caller. A failed redemption is coarse (unknown, spent, and expired are
// indistinguishable) so it reveals nothing.
func (h *Handler) redeem(w http.ResponseWriter, r *http.Request) {
	var req ticketRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Ticket == "" {
		http.Error(w, "ticket required", http.StatusBadRequest)
		return
	}
	tok, ttl, err := h.c.RedeemTicket(req.Ticket)
	if err != nil {
		if h.log != nil {
			h.log.Warn("ticket redemption rejected", "err", err)
		}
		http.Error(w, "invalid ticket", http.StatusUnauthorized)
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

// exchange is the RFC 8693-flavored token-exchange endpoint (docs/adr/0024). A
// service runtime authenticates (via the CallerAuthenticator) and exchanges its
// identity for a fresh job-scoped token, the pull complement to push-at-dispatch.
func (h *Handler) exchange(w http.ResponseWriter, r *http.Request) {
	// auth has already authenticated the caller and placed it on the context.
	caller, _ := callerFrom(r.Context())
	// A trusted control-plane caller (the allow-listed web tier, validated by
	// TokenReview) may request a token for any acting user, as the orchestrator's
	// own dispatch path does. A narrower per-service identity would set Trusted
	// false and carry a constrained authority.
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

// auth authenticates a request through the handler's CallerAuthenticator and puts
// the resulting Caller on the request context for the handler to read. A caller
// that cannot be authenticated is rejected (fail closed): the control API must
// never run open (docs/adr/0031).
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, err := h.caller.Authenticate(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, caller)))
	}
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
	runtimestats.Timing
}

// ticketRequest is the body of POST /control/redeem: a pull substrate's
// single-use redemption ticket (docs/adr/0029).
type ticketRequest struct {
	Ticket string `json:"ticket"`
}

type activeResponse struct {
	Active bool `json:"active"`
}

// TokenIssuer mints a job-scoped token for an authenticated caller. The
// orchestrator satisfies it via MintJobToken (docs/adr/0024).
type TokenIssuer interface {
	MintJobToken(jobType, actingUser string) (token string, expiresIn time.Duration, err error)
}

// Caller is the authenticated identity of a control-plane client.
type Caller struct {
	// Subject identifies the caller — its ServiceAccount username as validated by
	// TokenReview (e.g. "system:serviceaccount:zumble-zay:zumble-zay").
	Subject string
	// Trusted means the caller may request a token for any acting user, as the
	// orchestrator's own dispatch path does. A narrower per-service identity would
	// set this false and carry a constrained authority instead.
	Trusted bool
}

// CallerAuthenticator authenticates a control-plane request by the caller's own
// platform identity: a projected ServiceAccount token validated by TokenReview
// (docs/adr/0031, 0034). internal/controlauth implements it and is injected into
// NewHandler, so this package holds no Kubernetes client (docs/adr/0023).
type CallerAuthenticator interface {
	Authenticate(r *http.Request) (Caller, error)
}

// denyAllCaller rejects every request. It is the fail-closed default when a
// Handler is built without an authenticator, so a misconfiguration never leaves
// the control API open.
type denyAllCaller struct{}

func (denyAllCaller) Authenticate(*http.Request) (Caller, error) {
	return Caller{}, errUnauthorizedCaller
}

var errUnauthorizedCaller = errors.New("controlplane: unauthorized caller")

// callerCtxKey carries the authenticated Caller from auth to the route handler.
type callerCtxKey struct{}

// callerFrom returns the Caller that auth placed on the context.
func callerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerCtxKey{}).(Caller)
	return c, ok
}

type tokenRequest struct {
	JobType    string `json:"job_type"`
	ActingUser string `json:"acting_user"`
	// SubjectToken is the caller's own identity token (RFC 8693). The caller is
	// authenticated by its projected ServiceAccount token in the Authorization
	// header (TokenReview); this field is reserved for a future on-behalf-of flow.
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
