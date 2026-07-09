package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/vault"
)

func TestLogoutRevokesAndClearsGitHubCredential(t *testing.T) {
	var (
		revokeCalled bool
		gotToken     string
		gotUser      string
		gotPass      string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("revoke method = %s, want DELETE", r.Method)
		}
		revokeCalled = true
		gotUser, gotPass, _ = r.BasicAuth()
		var body struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotToken = body.AccessToken
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sessions := session.NewManager([]byte("test-session-secret-0123456789abcdef"), false)
	vlt := vault.NewMemoryVault()
	_ = vlt.Put(context.Background(), "github:123", vault.Credential{
		Provider: "github", AccessToken: "gho_livetoken",
	})

	h := &Handler{
		sessions:      sessions,
		vault:         vlt,
		client:        &http.Client{Timeout: 5 * time.Second},
		githubAPIBase: srv.URL,
		providers: map[string]*provider{
			"github": {name: "github", oauth: &oauth2.Config{ClientID: "cid", ClientSecret: "csecret"}},
		},
	}

	// Establish an authenticated session and capture its cookie.
	authRec := httptest.NewRecorder()
	sessions.Authenticate(authRec, httptest.NewRequest(http.MethodGet, "/", nil),
		&session.User{ID: "github:123", Provider: "github"})

	// Log out with that cookie.
	logoutReq := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	for _, c := range authRec.Result().Cookies() {
		logoutReq.AddCookie(c)
	}
	logoutRec := httptest.NewRecorder()
	h.Logout(logoutRec, logoutReq)

	if logoutRec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", logoutRec.Code)
	}
	// The vault credential must be gone.
	if _, err := vlt.Get(context.Background(), "github:123", "github"); err != vault.ErrNotFound {
		t.Errorf("vault Get after logout err = %v, want ErrNotFound", err)
	}
	// GitHub must have been asked to revoke the exact token with the app creds.
	if !revokeCalled {
		t.Fatal("expected a revocation call to GitHub")
	}
	if gotToken != "gho_livetoken" {
		t.Errorf("revoked token = %q, want gho_livetoken", gotToken)
	}
	if gotUser != "cid" || gotPass != "csecret" {
		t.Errorf("revoke basic auth = %q:%q, want cid:csecret", gotUser, gotPass)
	}
	// The session must be destroyed: the old cookie no longer resolves a user.
	check := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range authRec.Result().Cookies() {
		check.AddCookie(c)
	}
	if u := sessions.CurrentUser(check); u != nil {
		t.Errorf("session still active after logout: %+v", u)
	}
}

func TestCredentialRefreshesAndRotatesExpiredToken(t *testing.T) {
	var refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", r.FormValue("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-tok","refresh_token":"rotated-ref","token_type":"bearer","expires_in":28800}`))
	}))
	defer srv.Close()

	vlt := vault.NewMemoryVault()
	_ = vlt.Put(context.Background(), "u1", vault.Credential{
		Provider: "github", AccessToken: "stale-tok", RefreshToken: "old-ref",
		TokenType: "bearer", Expiry: time.Now().Add(-time.Hour),
	})

	h := &Handler{
		vault: vlt,
		providers: map[string]*provider{
			"github": {name: "github", oauth: &oauth2.Config{
				ClientID: "cid", ClientSecret: "secret",
				Endpoint: oauth2.Endpoint{TokenURL: srv.URL + "/token"},
			}},
		},
	}

	cred, err := h.Credential(context.Background(), "u1", "github")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.AccessToken != "fresh-tok" {
		t.Errorf("access token = %q, want fresh-tok", cred.AccessToken)
	}
	if cred.RefreshToken != "rotated-ref" {
		t.Errorf("refresh token = %q, want rotated-ref", cred.RefreshToken)
	}
	if refreshCalls == 0 {
		t.Error("expected a refresh call to the token endpoint")
	}

	// The rotation must be persisted so the next vend starts fresh.
	stored, _ := vlt.Get(context.Background(), "u1", "github")
	if stored.AccessToken != "fresh-tok" || stored.RefreshToken != "rotated-ref" {
		t.Errorf("vault not updated with rotated pair: %+v", stored)
	}
}

func TestCredentialPassesThroughWithoutRefreshToken(t *testing.T) {
	vlt := vault.NewMemoryVault()
	_ = vlt.Put(context.Background(), "u1", vault.Credential{
		Provider: "github", AccessToken: "long-lived",
	})
	h := &Handler{
		vault: vlt,
		providers: map[string]*provider{
			"github": {name: "github", oauth: &oauth2.Config{}},
		},
	}

	cred, err := h.Credential(context.Background(), "u1", "github")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.AccessToken != "long-lived" {
		t.Errorf("access token = %q, want long-lived (no refresh attempted)", cred.AccessToken)
	}
}

func TestCredentialValidTokenNotRefreshed(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	vlt := vault.NewMemoryVault()
	_ = vlt.Put(context.Background(), "u1", vault.Credential{
		Provider: "github", AccessToken: "good-tok", RefreshToken: "ref",
		TokenType: "bearer", Expiry: time.Now().Add(time.Hour),
	})
	h := &Handler{
		vault: vlt,
		providers: map[string]*provider{
			"github": {name: "github", oauth: &oauth2.Config{
				Endpoint: oauth2.Endpoint{TokenURL: srv.URL + "/token"},
			}},
		},
	}

	cred, err := h.Credential(context.Background(), "u1", "github")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.AccessToken != "good-tok" {
		t.Errorf("access token = %q, want good-tok (unchanged)", cred.AccessToken)
	}
	if calls != 0 {
		t.Errorf("token endpoint called %d times; a valid token must not be refreshed", calls)
	}
}
