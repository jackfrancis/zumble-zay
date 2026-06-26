package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/vault"
)

// CredentialSource returns a usable (fresh) provider credential for a user,
// refreshing and rotating it when the stored one has expired. It is the seam
// the vend endpoint depends on, so token-refresh policy lives with the OAuth
// provider configuration rather than in this handler (docs/adr/0006).
type CredentialSource interface {
	Credential(ctx context.Context, userID, provider string) (vault.Credential, error)
}

// CredentialHandler vends a delegated provider credential to an authorized
// agent runtime. ZZ is a credential broker, not a data broker: the runtime
// receives a short-lived credential and calls the provider directly; ZZ never
// proxies provider data (see docs/adr/0006).
type CredentialHandler struct {
	source CredentialSource
}

// NewCredentialHandler constructs a CredentialHandler over the given credential
// source. Refresh happens at vend time — the single point a credential leaves ZZ.
func NewCredentialHandler(source CredentialSource) *CredentialHandler {
	return &CredentialHandler{source: source}
}

// vendedCredential is what an agent receives. The refresh token is deliberately
// withheld: the runtime only needs the access token to call the provider, and
// ZZ keeps the refresh token to mint future credentials.
type vendedCredential struct {
	Provider    string `json:"provider"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type,omitempty"`
	Expiry      string `json:"expiry,omitempty"` // RFC3339; empty if the token does not expire
}

// Vend handles POST /agent/credentials/{provider}. It requires the
// signals:read scope and is restricted to workload principals, then returns the
// acting user's credential for the named provider.
func (h *CredentialHandler) Vend(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Credential vending is for agent runtimes only. An interactive browser
	// session must never be able to extract a raw provider token through ZZ.
	if p.Kind != principal.KindWorkload {
		writeError(w, http.StatusForbidden, "credential vending is for agent runtimes")
		return
	}
	provider := r.PathValue("provider")
	if provider == "" {
		writeError(w, http.StatusBadRequest, "provider required")
		return
	}

	cred, err := h.source.Credential(r.Context(), p.ActingUserID, provider)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			// The user has not consented, or the token was never retained.
			writeError(w, http.StatusNotFound, "no credential for user and provider")
			return
		}
		// Refresh failed (e.g. a revoked refresh token): the runtime cannot be
		// vended a usable credential.
		writeError(w, http.StatusBadGateway, "could not obtain credential")
		return
	}

	out := vendedCredential{
		Provider:    cred.Provider,
		AccessToken: cred.AccessToken,
		TokenType:   cred.TokenType,
	}
	if !cred.Expiry.IsZero() {
		out.Expiry = cred.Expiry.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}
