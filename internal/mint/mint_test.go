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
	m := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute)
	v := m.Verifier()
	tok, err := m.Mint(testClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := v.Verify(tok)
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
	m := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute)
	v := m.Verifier()
	tok, _ := m.Mint(testClaims())
	// Flip a character in the payload segment; the signature must no longer match.
	tampered := "x" + tok[1:]
	if _, err := v.Verify(tampered); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for tampered payload, got %v", err)
	}
}

func TestVerifyRejectsForeignKey(t *testing.T) {
	a := NewMinterFromSeed([]byte("secret-a-of-sufficient-length-aaa"), time.Minute)
	b := NewMinterFromSeed([]byte("secret-b-of-sufficient-length-bbb"), time.Minute)
	tok, _ := a.Mint(testClaims())
	// b's verifier holds a different public key, so a's token must not verify.
	if _, err := b.Verifier().Verify(tok); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for foreign key, got %v", err)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	m := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute)
	past := time.Now().Add(-time.Hour)
	m.now = func() time.Time { return past }
	tok, _ := m.Mint(testClaims())
	v := m.Verifier() // verifier uses the present, after the token's expiry
	if _, err := v.Verify(tok); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestValidateMapsToWorkloadPrincipal(t *testing.T) {
	m := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute)
	v := m.Verifier()
	tok, _ := m.Mint(testClaims())
	p, err := v.Validate(&http.Request{}, tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Kind != principal.KindWorkload {
		t.Fatalf("expected workload principal, got %q", p.Kind)
	}
	if p.ActingUserID != "github:123" {
		t.Fatalf("acting user mismatch: %q", p.ActingUserID)
	}
	if p.JobID != "job-1" {
		t.Fatalf("job id not carried onto the principal: %q", p.JobID)
	}
	if !p.HasScope(principal.ScopeMetadataWrite) || !p.HasScope(principal.ScopeSignalsRead) {
		t.Fatalf("scopes not carried: %+v", p.Scopes)
	}
	if p.HasScope(principal.ScopeAll) {
		t.Fatalf("workload must not hold ScopeAll")
	}
}

func TestMintStampsAgentAudience(t *testing.T) {
	m := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute)
	tok, _ := m.Mint(testClaims())
	c, err := m.Verifier().Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Audience != AudienceAgent {
		t.Fatalf("Mint must stamp the agent audience, got %q", c.Audience)
	}
}

func TestValidateRejectsForeignAudience(t *testing.T) {
	m := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute)
	c := testClaims()
	c.Audience = "some-other-plane" // non-empty, so Mint keeps it rather than defaulting
	tok, _ := m.Mint(c)
	if _, err := m.Verifier().Validate(&http.Request{}, tok); err != ErrInvalidToken {
		t.Fatalf("Validate must reject a token minted for a foreign audience, got %v", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	v := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute).Verifier()
	for _, tok := range []string{"", "nodot", ".", "a.", ".b", "a.b.c"} {
		if _, err := v.Verify(tok); err == nil {
			t.Fatalf("expected error for malformed token %q", tok)
		}
	}
}

func TestVerifierIsSeparableFromMinter(t *testing.T) {
	// The split deployment hands the web tier only a public key: a Verifier built
	// from the minter's public key validates tokens, with no access to the private
	// key, matching the orchestrator-signs / web-verifies boundary (docs/adr/0023).
	m := NewMinterFromSeed([]byte("test-secret-of-sufficient-length!"), time.Minute)
	v := NewVerifier(m.Public())
	tok, _ := m.Mint(testClaims())
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("public-key Verifier rejected a valid token: %v", err)
	}
}
