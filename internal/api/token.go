package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// TicketRedeemer exchanges a runtime's single-use ticket for its job token
// (docs/adr/0029). controlplane.Client satisfies it; depending on the seam keeps
// this package off the control plane's concrete type, as the completion handler
// does for its reporter.
type TicketRedeemer interface {
	RedeemTicket(ctx context.Context, ticket string) (token string, expiresIn int, err error)
}

// TokenHandler serves POST /agent/token: a pull-substrate runtime (kagent,
// docs/adr/0029) redeems the single-use ticket it received in place of a token
// for the actual job token. Unlike the other /agent/* routes this one is NOT
// behind the job-token auth — the runtime has no token yet, and the ticket is the
// authorization (single-use, and the orchestrator issues exactly one per
// dispatched job). Redemption is forwarded to the orchestrator, which mints a
// token whose job id matches the dispatched job so completion still correlates.
type TokenHandler struct {
	redeemer TicketRedeemer
	log      *slog.Logger
}

// NewTokenHandler constructs a TokenHandler that redeems via redeemer.
func NewTokenHandler(redeemer TicketRedeemer, log *slog.Logger) *TokenHandler {
	return &TokenHandler{redeemer: redeemer, log: log}
}

type redeemRequest struct {
	Ticket string `json:"ticket"`
}

type redeemResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// Redeem handles POST /agent/token. It returns the job token for a valid, unspent
// ticket; an unknown, spent, or expired ticket is a coarse 401 that reveals
// nothing about why. The ticket rides the request body, never a header, and is
// consumed on first use.
func (h *TokenHandler) Redeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Ticket == "" {
		writeError(w, http.StatusBadRequest, "ticket required")
		return
	}
	tok, exp, err := h.redeemer.RedeemTicket(r.Context(), req.Ticket)
	if err != nil {
		if h.log != nil {
			h.log.Warn("ticket redemption failed", "err", err)
		}
		writeError(w, http.StatusUnauthorized, "invalid ticket")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(redeemResponse{AccessToken: tok, TokenType: "Bearer", ExpiresIn: exp})
}
