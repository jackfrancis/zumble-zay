package mint

import (
	"net/http"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/principal"
)

func testClaims() Claims {
	return Claims{
		Subject:      "runtime-abc",
		ActingUserID: "github:123",
		Scopes:       []principal.Scope{principal.ScopeSignalsRead, principal.ScopeMetadataWrite},
		JobID:        "job-1",
		Provider:     "github",
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	m := NewMinter([]byte("test-secret-of-sufficient-length!"), time.Minute)
	tok, err := m.Mint(testClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "runtime-abc" || got.ActingUserID != "github:123" || got.JobID != "job-1" {
		t.Fatalf("claims round-tripped wrong: %+v", got)
	}
	if got.IssuedAt == 0 || got.ExpiresAt <= got.IssuedAt {
		t.Fatalf("issue/expiry not stamped: iat=%d exp=%d", got.IssuedAt, got.ExpiresAt)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	m := NewMinter([]byte("test-secret-of-sufficient-length!"), time.Minute)
	tok, _ := m.Mint(testClaims())
	// Flip a character in the payload segment; the signature must no longer match.
	tampered := "x" + tok[1:]
	if _, err := m.Verify(tampered); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for tampered payload, got %v", err)
	}
}

func TestVerifyRejectsForeignKey(t *testing.T) {
	a := NewMinter([]byte("secret-a-of-sufficient-length-aaa"), time.Minute)
	b := NewMinter([]byte("secret-b-of-sufficient-length-bbb"), time.Minute)
	tok, _ := a.Mint(testClaims())
	if _, err := b.Verify(tok); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for foreign key, got %v", err)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	m := NewMinter([]byte("test-secret-of-sufficient-length!"), time.Minute)
	past := time.Now().Add(-time.Hour)
	m.now = func() time.Time { return past }
	tok, _ := m.Mint(testClaims())
	m.now = time.Now // back to the present, after the token's expiry
	if _, err := m.Verify(tok); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestValidateMapsToWorkloadPrincipal(t *testing.T) {
	m := NewMinter([]byte("test-secret-of-sufficient-length!"), time.Minute)
	tok, _ := m.Mint(testClaims())
	p, err := m.Validate(&http.Request{}, tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Kind != principal.KindWorkload {
		t.Fatalf("expected workload principal, got %q", p.Kind)
	}
	if p.ActingUserID != "github:123" {
		t.Fatalf("acting user mismatch: %q", p.ActingUserID)
	}
	if !p.HasScope(principal.ScopeMetadataWrite) || !p.HasScope(principal.ScopeSignalsRead) {
		t.Fatalf("scopes not carried: %+v", p.Scopes)
	}
	if p.HasScope(principal.ScopeAll) {
		t.Fatalf("workload must not hold ScopeAll")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	m := NewMinter([]byte("test-secret-of-sufficient-length!"), time.Minute)
	for _, tok := range []string{"", "nodot", ".", "a.", ".b", "a.b.c"} {
		if _, err := m.Verify(tok); err == nil {
			t.Fatalf("expected error for malformed token %q", tok)
		}
	}
}
