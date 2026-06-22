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
	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// New builds the fully wired HTTP handler for the application.
func New(cfg *config.Config, log *slog.Logger) http.Handler {
	sessions := session.NewManager(cfg.SessionSecret, cfg.CookieSecure)
	authH := auth.NewHandler(cfg, sessions)
	// No workload token validator yet; cookie sessions only until ZZ token
	// issuance is built. The seam is ready for it.
	authenticator := authn.New(sessions, nil)

	// Worklist domain. In-memory store + no-op ingestor for now; both are
	// behind interfaces so the cloud store and the agentic backfill plug in
	// without changing the handler.
	worklistHandler := api.NewWorklistHandler(worklist.NewMemoryStore(), worklist.NoopIngestor{Log: log})

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

	// Global middleware chain (outermost first).
	var h http.Handler = mux
	h = cors(cfg.AllowedOrigins, h)
	h = securityHeaders(h)
	h = logRequests(log, h)
	h = recoverer(log, h)
	return h
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
