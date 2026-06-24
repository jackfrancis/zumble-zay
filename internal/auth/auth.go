// Package auth wires OAuth2 login with trusted identity providers
// (Google, GitHub, Microsoft) on top of the session manager.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/microsoft"

	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/vault"
)

// provider describes how to authenticate with a single identity provider and
// how to map its user-info response onto a session.User.
type provider struct {
	name      string
	oauth     *oauth2.Config
	userURL   string
	userAgent string // some providers (GitHub) require a User-Agent header
	mapUser   func(raw map[string]any) (*session.User, error)
}

// Handler exposes HTTP handlers for the OAuth login lifecycle.
type Handler struct {
	sessions  *session.Manager
	providers map[string]*provider
	client    *http.Client
	vault     vault.Vault
}

// NewHandler builds the auth handler from configuration. Only providers with
// credentials configured are registered. The vault retains each user's
// delegated provider token so agent runtimes can later be vended a credential
// to act on their behalf (ADR 0006).
func NewHandler(cfg *config.Config, sessions *session.Manager, vlt vault.Vault) *Handler {
	h := &Handler{
		sessions:  sessions,
		providers: make(map[string]*provider),
		client:    &http.Client{Timeout: 10 * time.Second},
		vault:     vlt,
	}

	redirect := func(name string) string {
		return cfg.BaseURL + "/auth/" + name + "/callback"
	}

	if app := cfg.Providers.Google; app.Enabled() {
		h.providers["google"] = &provider{
			name: "google",
			oauth: &oauth2.Config{
				ClientID:     app.ClientID,
				ClientSecret: app.ClientSecret,
				Endpoint:     google.Endpoint,
				RedirectURL:  redirect("google"),
				Scopes:       []string{"openid", "email", "profile"},
			},
			userURL: "https://openidconnect.googleapis.com/v1/userinfo",
			mapUser: func(raw map[string]any) (*session.User, error) {
				sub, _ := raw["sub"].(string)
				if sub == "" {
					return nil, fmt.Errorf("google: missing subject")
				}
				email, _ := raw["email"].(string)
				name, _ := raw["name"].(string)
				return &session.User{ID: "google:" + sub, Provider: "google", Email: email, Name: name}, nil
			},
		}
	}

	if app := cfg.Providers.GitHub; app.Enabled() {
		h.providers["github"] = &provider{
			name: "github",
			oauth: &oauth2.Config{
				ClientID:     app.ClientID,
				ClientSecret: app.ClientSecret,
				Endpoint:     github.Endpoint,
				RedirectURL:  redirect("github"),
				Scopes:       []string{"read:user", "user:email"},
			},
			userURL:   "https://api.github.com/user",
			userAgent: "zumble-zay",
			mapUser: func(raw map[string]any) (*session.User, error) {
				id, ok := raw["id"].(float64)
				if !ok {
					return nil, fmt.Errorf("github: missing id")
				}
				email, _ := raw["email"].(string)
				name, _ := raw["name"].(string)
				if name == "" {
					name, _ = raw["login"].(string)
				}
				return &session.User{
					ID:       "github:" + strconv.FormatInt(int64(id), 10),
					Provider: "github",
					Email:    email,
					Name:     name,
				}, nil
			},
		}
	}

	if app := cfg.Providers.Microsoft; app.Enabled() {
		h.providers["microsoft"] = &provider{
			name: "microsoft",
			oauth: &oauth2.Config{
				ClientID:     app.ClientID,
				ClientSecret: app.ClientSecret,
				Endpoint:     microsoft.AzureADEndpoint(cfg.Providers.MicrosoftTenant),
				RedirectURL:  redirect("microsoft"),
				Scopes:       []string{"openid", "email", "profile", "User.Read"},
			},
			userURL: "https://graph.microsoft.com/v1.0/me",
			mapUser: func(raw map[string]any) (*session.User, error) {
				id, _ := raw["id"].(string)
				if id == "" {
					return nil, fmt.Errorf("microsoft: missing id")
				}
				email, _ := raw["mail"].(string)
				if email == "" {
					email, _ = raw["userPrincipalName"].(string)
				}
				name, _ := raw["displayName"].(string)
				return &session.User{ID: "microsoft:" + id, Provider: "microsoft", Email: email, Name: name}, nil
			},
		}
	}

	return h
}

// Providers returns the names of the enabled providers.
func (h *Handler) Providers() []string {
	names := make([]string, 0, len(h.providers))
	for name := range h.providers {
		names = append(names, name)
	}
	return names
}

// Login starts the OAuth flow for the provider named in the path value
// "provider", redirecting the user to the provider's consent screen.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[r.PathValue("provider")]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	state := session.NewToken()
	verifier := oauth2.GenerateVerifier()
	h.sessions.StartOAuth(w, &session.OAuthFlow{
		Provider: p.name,
		State:    state,
		Verifier: verifier,
	})

	url := p.oauth.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the OAuth flow: it validates state, exchanges the code,
// fetches the user profile, and establishes an authenticated session.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[r.PathValue("provider")]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	flow := h.sessions.OAuthFlow(r)
	if flow == nil || flow.Provider != p.name {
		http.Error(w, "no login in progress", http.StatusBadRequest)
		return
	}
	if state := r.URL.Query().Get("state"); state == "" || state != flow.State {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, h.client)

	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	raw, err := h.fetchUser(ctx, p, token)
	if err != nil {
		http.Error(w, "failed to fetch user profile", http.StatusBadGateway)
		return
	}
	user, err := p.mapUser(raw)
	if err != nil {
		http.Error(w, "invalid user profile", http.StatusBadGateway)
		return
	}

	// Retain the delegated provider token in the vault so agent runtimes can be
	// vended a credential to act on the user's behalf (ADR 0006). Best-effort:
	// a vault failure must not block interactive login.
	if h.vault != nil {
		_ = h.vault.Put(ctx, user.ID, vault.Credential{
			Provider:     p.name,
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenType:    token.TokenType,
			Expiry:       token.Expiry,
		})
	}

	h.sessions.Authenticate(w, r, user)
	http.Redirect(w, r, "/", http.StatusFound)
}

// fetchUser calls the provider's user-info endpoint with the access token.
func (h *Handler) fetchUser(ctx context.Context, p *provider, token *oauth2.Token) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userURL, nil)
	if err != nil {
		return nil, err
	}
	token.SetAuthHeader(req)
	req.Header.Set("Accept", "application/json")
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s userinfo: status %d", p.name, resp.StatusCode)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	// GitHub may omit the email from /user when it is private; fall back to
	// the dedicated emails endpoint to find the primary verified address.
	if p.name == "github" {
		if email, _ := raw["email"].(string); email == "" {
			if email := h.githubPrimaryEmail(ctx, token); email != "" {
				raw["email"] = email
			}
		}
	}
	return raw, nil
}

// githubPrimaryEmail looks up the user's primary, verified email address.
func (h *Handler) githubPrimaryEmail(ctx context.Context, token *oauth2.Token) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/emails", nil)
	if err != nil {
		return ""
	}
	token.SetAuthHeader(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zumble-zay")

	resp, err := h.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &emails); err != nil {
		return ""
	}
	for _, e := range emails {
		if e.Primary && e.Verified && !strings.HasSuffix(e.Email, "@users.noreply.github.com") {
			return e.Email
		}
	}
	return ""
}
