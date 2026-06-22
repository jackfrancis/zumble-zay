// Package authn resolves a principal.Principal for each request from either an
// interactive session cookie or a workload bearer token, and provides
// authorization middleware (RequireAuth, RequireScope).
package authn

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/session"
)

// ErrNoToken indicates a bearer token could not be validated.
var ErrNoToken = errors.New("authn: no valid token")

// TokenValidator validates a workload bearer token and returns its principal.
//
// It is the seam for ZZ-issued, short-lived runtime tokens. Until token
// issuance is implemented, the default validator rejects all tokens.
type TokenValidator interface {
	Validate(r *http.Request, token string) (*principal.Principal, error)
}

// noTokenValidator rejects every token; the default until issuance exists.
type noTokenValidator struct{}

func (noTokenValidator) Validate(*http.Request, string) (*principal.Principal, error) {
	return nil, ErrNoToken
}

// Authenticator resolves principals and enforces authorization.
type Authenticator struct {
	sessions *session.Manager
	tokens   TokenValidator
}

// New builds an Authenticator. If tokens is nil, bearer tokens are rejected
// (cookie sessions still work), which is the expected default for now.
func New(sessions *session.Manager, tokens TokenValidator) *Authenticator {
	if tokens == nil {
		tokens = noTokenValidator{}
	}
	return &Authenticator{sessions: sessions, tokens: tokens}
}

// resolve returns the principal for a request, or nil if unauthenticated.
func (a *Authenticator) resolve(r *http.Request) *principal.Principal {
	// Workload path: a presented bearer token is authoritative. An invalid
	// token must not silently fall through to the cookie session.
	if tok := bearerToken(r); tok != "" {
		p, err := a.tokens.Validate(r, tok)
		if err != nil || p == nil {
			return nil
		}
		return p
	}

	// Interactive path: OAuth session cookie. Users act on their own data with
	// full scope within their namespace.
	if u := a.sessions.CurrentUser(r); u != nil {
		return &principal.Principal{
			Kind:         principal.KindUser,
			Subject:      u.ID,
			ActingUserID: u.ID,
			Scopes:       []principal.Scope{principal.ScopeAll},
		}
	}
	return nil
}

// RequireAuth rejects unauthenticated requests and injects the principal into
// the request context for downstream handlers.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := a.resolve(r)
		if p == nil {
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.NewContext(r.Context(), p)))
	})
}

// RequireScope rejects requests whose principal lacks the given scope.
func (a *Authenticator) RequireScope(s principal.Scope, next http.Handler) http.Handler {
	return a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal.FromContext(r.Context())
		if !p.HasScope(s) {
			writeJSONError(w, http.StatusForbidden, "insufficient scope")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. Tokens are accepted from this header only — never query strings.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
