package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/principal"
	"github.com/jackfrancis/zumble-zay/internal/session"
)

// newAuthenticatedRequest returns a request carrying a valid session cookie for
// the given user, plus the session manager that issued it.
func newAuthenticatedRequest(t *testing.T, mgr *session.Manager, u *session.User) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	mgr.Authenticate(rec, httptest.NewRequest(http.MethodGet, "/", nil), u)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequireAuthRejectsAnonymous(t *testing.T) {
	mgr := session.NewManager([]byte("test-secret-至少-32-bytes-长度需要满足"), false)
	a := New(mgr, nil)

	rec := httptest.NewRecorder()
	a.RequireAuth(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestRequireAuthAllowsSessionAndInjectsPrincipal(t *testing.T) {
	mgr := session.NewManager([]byte("test-secret-至少-32-bytes-长度需要满足"), false)
	a := New(mgr, nil)

	var got *principal.Principal
	h := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = principal.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := newAuthenticatedRequest(t, mgr, &session.User{ID: "github:42", Provider: "github", Name: "Octo"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got == nil {
		t.Fatal("principal not injected into context")
	}
	if got.Kind != principal.KindUser || got.Subject != "github:42" || got.ActingUserID != "github:42" {
		t.Fatalf("unexpected principal: %+v", got)
	}
	if !got.HasScope(principal.ScopeMetadataWrite) {
		t.Fatal("interactive user should hold ScopeAll and satisfy any scope")
	}
}

// stubValidator is a TokenValidator that returns a fixed principal for a known
// token and an error otherwise.
type stubValidator struct {
	token string
	p     *principal.Principal
}

func (s stubValidator) Validate(_ *http.Request, token string) (*principal.Principal, error) {
	if token == s.token {
		return s.p, nil
	}
	return nil, ErrNoToken
}

func TestBearerTokenPath(t *testing.T) {
	mgr := session.NewManager([]byte("test-secret-至少-32-bytes-长度需要满足"), false)
	workload := &principal.Principal{
		Kind:         principal.KindWorkload,
		Subject:      "runtime:abc",
		ActingUserID: "github:42",
		Scopes:       []principal.Scope{principal.ScopeSignalsRead},
	}
	a := New(mgr, stubValidator{token: "good-token", p: workload})

	t.Run("valid token authorizes scope", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer good-token")
		a.RequireScope(principal.ScopeSignalsRead, okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("valid token lacking scope is forbidden", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer good-token")
		a.RequireScope(principal.ScopeMetadataWrite, okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	})

	t.Run("invalid token does not fall through to session", func(t *testing.T) {
		// Request carries BOTH a bad bearer token and a valid session cookie;
		// the bad token must win and produce 401, never the cookie.
		req := newAuthenticatedRequest(t, mgr, &session.User{ID: "github:42"})
		req.Header.Set("Authorization", "Bearer bad-token")
		rec := httptest.NewRecorder()
		a.RequireAuth(okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", rec.Code)
		}
	})
}

func TestHasScope(t *testing.T) {
	user := &principal.Principal{Scopes: []principal.Scope{principal.ScopeAll}}
	if !user.HasScope(principal.ScopeMetadataWrite) {
		t.Fatal("ScopeAll must satisfy any scope")
	}
	limited := &principal.Principal{Scopes: []principal.Scope{principal.ScopeSignalsRead}}
	if limited.HasScope(principal.ScopeMetadataWrite) {
		t.Fatal("limited principal must not satisfy unheld scope")
	}
	if !limited.HasScope(principal.ScopeSignalsRead) {
		t.Fatal("limited principal must satisfy held scope")
	}
}
