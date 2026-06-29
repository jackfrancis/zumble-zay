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
	"github.com/jackfrancis/zumble-zay/internal/reconcile"
	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/vault"
	"github.com/jackfrancis/zumble-zay/internal/webui"
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
	credentialHandler := api.NewCredentialHandler(authH)
	ingestHandler := api.NewIngestHandler(store)
	// The assistive conversation runs as an ephemeral converse runtime spawned by
	// the orchestrator (docs/adr/0019); the HTTP layer only enqueues turns. It is
	// available when a chat model is configured.
	convEnabled := cfg.AI.Token != ""
	threadHandler := api.NewThreadHandler(store, orch, convEnabled)
	agentThreadHandler := api.NewAgentThreadHandler(store)
	agentResearchHandler := api.NewAgentResearchHandler(store)
	webHandler := webui.New(sessions, store, orch, authH, convEnabled)

	mux := http.NewServeMux()

	// Health check.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Landing page (server-rendered) and its static assets (docs/adr/0016).
	mux.Handle("GET /{$}", http.HandlerFunc(webHandler.Index))
	mux.Handle("GET /static/", webHandler.Static())
	// Hide an item from the landing page (docs/adr/0017). The handler checks the
	// session itself and uses Post/Redirect/Get.
	mux.Handle("POST /items/hide", http.HandlerFunc(webHandler.Hide))
	// Per-item assistive conversation (docs/adr/0018, 0019): the thread page, the
	// JSON turn endpoint the page posts to, and the poll endpoint it reads. A turn
	// is answered asynchronously by a spawned converse runtime.
	mux.Handle("GET /items/thread", http.HandlerFunc(webHandler.Thread))
	mux.Handle("POST /api/thread", authenticator.RequireAuth(http.HandlerFunc(threadHandler.Post)))
	mux.Handle("GET /api/thread", authenticator.RequireAuth(http.HandlerFunc(threadHandler.Get)))
	mux.Handle("POST /api/thread/resume", authenticator.RequireAuth(http.HandlerFunc(threadHandler.Resume)))

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
	// Converse write-back: a spawned converse runtime posts the assistant's reply
	// for an item here (docs/adr/0019).
	mux.Handle("POST /agent/thread", authenticator.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(agentThreadHandler.Append)))
	// Research write-back: a spawned github-research runtime posts the per-axis
	// re-weighting multipliers for an item here (docs/adr/0022).
	mux.Handle("POST /agent/research", authenticator.RequireScope(principal.ScopeMetadataWrite, http.HandlerFunc(agentResearchHandler.Set)))
	// Read path: a runtime reads its acting user's persisted work to augment it
	// in place (docs/adr/0010) rather than re-deriving it from the provider.
	mux.Handle("GET /agent/worklist", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(ingestHandler.List)))

	// Global middleware chain (outermost first).
	var h http.Handler = mux
	h = cors(cfg.AllowedOrigins, h)
	h = securityHeaders(h)
	h = logRequests(log, h)
	h = recoverer(log, h)

	// Staleness reconciler: when the store can enumerate items, periodically
	// re-rank those whose discussion-derived research has gone stale, enqueuing
	// per-item research jobs through the orchestrator (docs/adr/0022). At
	// replicas:1 it is a single goroutine; leader-gate it past that (ADR 0007).
	stopReconcile := func() {}
	if lister, ok := store.(worklist.Lister); ok {
		rec := reconcile.New(lister, orch, reconcile.DefaultInterval, log)
		rec.Start()
		stopReconcile = rec.Stop
	}

	cleanup := func() {
		stopReconcile()
		orch.Stop()
	}
	return h, cleanup
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
