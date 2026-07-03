// Package server wires together routing, middleware, and handlers.
package server

import (
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackfrancis/zumble-zay/internal/api"
	"github.com/jackfrancis/zumble-zay/internal/auth"
	"github.com/jackfrancis/zumble-zay/internal/authn"
	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/controlplane"
	"github.com/jackfrancis/zumble-zay/internal/mint"
	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/reconcile"
	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/vault"
	"github.com/jackfrancis/zumble-zay/internal/webui"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// New builds the fully wired HTTP handler for the web tier. The control plane is
// injected as a controlplane.Client: a co-located orchestrator (single-process)
// or the remote orchestrator's control API (the split deployment, docs/adr/0023).
// ZZ core imports no provider client (docs/adr/0006) and no launcher. The
// returned cleanup stops the staleness reconciler.
func New(cfg *config.Config, log *slog.Logger, cp controlplane.Client) (http.Handler, func()) {
	h, _, stop := newWithDeps(cfg, log, cp, vault.NewMemoryVault(), worklist.NewMemoryStore())
	return h, stop
}

// newWithDeps wires the handler over injected dependencies. Tests use it to seed
// a vault credential, share the store, and mint interactive sessions against the
// returned manager; New supplies in-memory defaults.
func newWithDeps(cfg *config.Config, log *slog.Logger, cp controlplane.Client, vlt vault.Vault, store worklist.Store) (http.Handler, *session.Manager, func()) {
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure)
	// The auth handler writes delegated provider tokens to the vault at login;
	// the credential-vend endpoint reads them for agent runtimes.
	authH := auth.NewHandler(cfg, sessions, vlt)
	// The web tier holds only the job-token verification key: it authenticates a
	// runtime's bearer but cannot mint one. The orchestrator is the sole issuer
	// (docs/adr/0023).
	authenticator := authn.New(sessions, mint.NewVerifier(verifierKey(cfg)))

	// The control plane (a co-located orchestrator or the remote control API)
	// implements worklist.Ingestor, so an empty worklist GET triggers an agentic
	// backfill; it also schedules conversation and research jobs. One store is
	// shared by the read path (GET /api/worklist) and the agent write path
	// (POST /agent/worklist).
	worklistHandler := api.NewWorklistHandler(store, cp)
	credentialHandler := api.NewCredentialHandler(authH)
	ingestHandler := api.NewIngestHandler(store, cfg.BotReviewers)
	// The assistive conversation runs as an ephemeral converse runtime spawned by
	// the orchestrator (docs/adr/0019); the HTTP layer only enqueues turns. It is
	// available when a chat model is configured.
	convEnabled := cfg.AI.Token != ""
	threadHandler := api.NewThreadHandler(store, cp, convEnabled)
	agentThreadHandler := api.NewAgentThreadHandler(store)
	agentResearchHandler := api.NewAgentResearchHandler(store)
	// A runtime reports terminal completion here; the web tier forwards it to the
	// orchestrator so the job finalizes immediately (docs/adr/0024).
	completeHandler := api.NewCompleteHandler(cp, log)
	// A pull-substrate runtime (kagent) redeems its single-use ticket for the job
	// token here; the web tier forwards to the orchestrator (docs/adr/0029).
	tokenHandler := api.NewTokenHandler(cp, log)
	webHandler := webui.New(sessions, store, cp, authH, convEnabled)

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
	mux.Handle("POST /api/thread/read", authenticator.RequireAuth(http.HandlerFunc(threadHandler.MarkRead)))

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
	// Completion report: a runtime tells ZZ its job finished; the web tier forwards
	// it to the orchestrator (docs/adr/0024). Any authenticated workload may report
	// its own job (identified by the token's job id), so it needs only signals:read.
	mux.Handle("POST /agent/complete", authenticator.RequireScope(principal.ScopeSignalsRead, http.HandlerFunc(completeHandler.Complete)))
	// Ticket redemption for the pull-path (docs/adr/0029): a pull-substrate runtime
	// exchanges its single-use ticket for the job token. It has no token yet, so
	// this route is not behind RequireScope — the ticket is the authorization, and
	// the orchestrator issues exactly one per dispatched job.
	mux.Handle("POST /agent/token", http.HandlerFunc(tokenHandler.Redeem))
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
	// per-item research jobs through the control plane (docs/adr/0022). A slower
	// Refresher re-ingests each owner's worklist so it stays in sync with GitHub —
	// new items appear, signals refresh, and completed work is retired
	// (docs/adr/0017); without it the pipeline runs only on an empty worklist. Both
	// live in the web tier because they read the (in-memory) store; they move to
	// the orchestrator once the store is shared (roadmap #5). At replicas:1 each is
	// a single goroutine; leader-gate them past that (ADR 0007).
	stopReconcile := func() {}
	if lister, ok := store.(worklist.Lister); ok {
		rec := reconcile.New(lister, cp, reconcile.DefaultInterval, log)
		rec.Start()
		ref := reconcile.NewRefresher(lister, cp, reconcile.DefaultRefreshInterval, log)
		ref.Start()
		stopReconcile = func() { rec.Stop(); ref.Stop() }
	}

	// The orchestrator's own lifecycle is owned by the composition root (the
	// co-located one in cmd/server, or a separate orchestrator process); the web
	// tier owns only the reconciler it starts here.
	cleanup := stopReconcile
	return h, sessions, cleanup
}

// verifierKey returns the Ed25519 public key authn uses to validate job tokens.
// An explicitly configured key wins; otherwise it is derived from the session
// secret so a single-process run needs no extra configuration (docs/adr/0023).
func verifierKey(cfg *config.Config) ed25519.PublicKey {
	if len(cfg.MintPublicKey) == ed25519.PublicKeySize {
		return cfg.MintPublicKey
	}
	_, pub := mint.KeyPairFromSeed(cfg.SessionSecret)
	return pub
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
