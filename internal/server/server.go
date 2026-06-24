// Package server wires together routing, middleware, and handlers.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackfrancis/zumble-zay/internal/api"
	"github.com/jackfrancis/zumble-zay/internal/auth"
	"github.com/jackfrancis/zumble-zay/internal/authn"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/mint"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/vault"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// New builds the fully wired HTTP handler for the application. The launcher is
// the agent-runtime substrate (in-process today); it is injected so ZZ core
// never imports a provider client (docs/adr/0006). The returned cleanup stops
// the orchestrator's workers.
func New(cfg *config.Config, log *slog.Logger, launcher orchestrator.Launcher) (http.Handler, func()) {
	return newWithDeps(cfg, log, launcher, vault.NewMemoryVault(), worklist.NewMemoryStore())
}

// newWithDeps wires the handler over injected dependencies. Tests use it to
// seed a vault credential and share the store; New supplies in-memory defaults.
func newWithDeps(cfg *config.Config, log *slog.Logger, launcher orchestrator.Launcher, vlt vault.Vault, store worklist.Store) (http.Handler, func()) {
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure)
	// The auth handler writes delegated provider tokens to the vault at login;
	// the credential-vend endpoint reads them for agent runtimes.
	authH := auth.NewHandler(cfg, sessions, vlt)
	// Minter issues and validates ZZ job tokens for agent runtimes; it is both
	// the orchestrator's minter and authn's workload TokenValidator.
	minter := mint.NewMinter(cfg.SessionSecret, 0)
	authenticator := authn.New(sessions, minter)

	// Co-located orchestrator: it implements worklist.Ingestor, so an empty
	// worklist GET triggers an agentic backfill. It mints job tokens with the
	// same minter authn validates (docs/adr/0007).
	orch := orchestrator.New(minter, launcher, log)

	// One store is shared by the read path (GET /api/worklist) and the agent
	// write path (POST /agent/worklist).
	worklistHandler := api.NewWorklistHandler(store, orch)
	credentialHandler := api.NewCredentialHandler(vlt)
	ingestHandler := api.NewIngestHandler(store)

	mux := http.NewServeMux()

	// Health check.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Auth lifecycle.
	mux.HandleFunc("GET /auth/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"providers": authH.Providers()})
	})
	mux.HandleFunc("GET /auth/{provider}/login", authH.Login)
	mux.HandleFunc("GET /auth/{provider}/callback", authH.Callback)
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		sessions.Destroy(w, r)
		w.WriteHeader(http.StatusNoContent)
	})

	// Current principal (interactive user today; workloads once tokens exist).
	mux.Handle("GET /api/me", authenticator.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal.FromContext(r.Context())
		resp := map[string]any{
			"kind":           p.Kind,
			"subject":        p.Subject,
			"acting_user_id": p.ActingUserID,
			"scopes":         p.Scopes,
		}
		// Enrich with profile fields for interactive users.
		if p.Kind == principal.KindUser {
			if u := sessions.CurrentUser(r); u != nil {
				resp["provider"] = u.Provider
				resp["email"] = u.Email
				resp["name"] = u.Name
			}
		}
		writeJSON(w, resp)
	})))

	// Worklist: the ordered set of work for the landing page.
	mux.Handle("GET /api/worklist", authenticator.RequireAuth(http.HandlerFunc(worklistHandler.List)))

	// Agent plane (workload tokens only). Credential vend lets a runtime obtain
	// the acting user's provider credential to call the provider directly;
	// ingest is the runtime's output sink back into ZZ.
	mux.Handle("POST /agent/credentials/{provider}", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(credentialHandler.Vend)))
	mux.Handle("POST /agent/worklist", authenticator.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(ingestHandler.Ingest)))

	// Global middleware chain (outermost first).
	var h http.Handler = mux
	h = cors(cfg.AllowedOrigins, h)
	h = securityHeaders(h)
	h = logRequests(log, h)
	h = recoverer(log, h)
	return h, orch.Stop
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
