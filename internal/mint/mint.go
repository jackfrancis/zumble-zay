// Package mint issues and validates ZZ job tokens: short-lived, job-scoped
// bearer tokens carried by ephemeral agent runtimes (see docs/adr/0001 and
// docs/adr/0002). The token is self-describing — its claims encode the acting
// user, the granted scopes, and an expiry — so ZZ authorizes a request from the
// token alone.
//
// The token is a compact Ed25519-signed envelope (base64url(claims).base64url(sig)),
// implemented with the standard library so ZZ takes on no JWT dependency. Signing
// is asymmetric so the issuer and the verifier are separate roles (docs/adr/0023):
// the orchestrator holds the private key and is the sole issuer (Minter); the
// internet-facing web tier holds only the public key and can verify a runtime's
// token but never mint one (Verifier).
package mint

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/principal"
)

// Errors returned by Verify/Validate. They are deliberately coarse so callers
// cannot distinguish tampering from expiry from malformed input.
var (
	ErrInvalidToken = errors.New("mint: invalid token")
	ErrExpired      = errors.New("mint: token expired")
)

// defaultTTL bounds how long a job token is valid. Jobs are expected to be
// short; a compromised runtime holds a credential only this long.
const defaultTTL = 10 * time.Minute

// AudienceAgent is the audience stamped on every job token. It binds the token
// to the runtime->web agent plane (the /agent/* routes) so a leaked runtime
// credential cannot be replayed on the interactive /api/* plane (docs/adr/0032).
// It is the token-level analog of the control plane's audience binding
// (docs/adr/0031).
const AudienceAgent = "zumble-zay-agent"

// Claims are the self-describing contents of a job token. Mint sets IssuedAt
// and ExpiresAt; the caller supplies the rest.
type Claims struct {
	Subject      string            `json:"sub"`            // ephemeral runtime id
	ActingUserID string            `json:"auid"`           // user the runtime acts for
	Scopes       []principal.Scope `json:"scopes"`         // granted capabilities
	JobID        string            `json:"jid"`            // 1:1 with the spawned job
	Provider     string            `json:"prov,omitempty"` // provider the job targets
	Audience     string            `json:"aud,omitempty"`  // plane the token is valid for (AudienceAgent)
	IssuedAt     int64             `json:"iat"`
	ExpiresAt    int64             `json:"exp"`
}

// Minter signs job tokens with an Ed25519 private key. It is the sole issuer of
// job tokens; only the orchestrator — the authorization server (docs/adr/0001,
// 0023) — holds one. Verification is a separate role (Verifier).
type Minter struct {
	key ed25519.PrivateKey
	ttl time.Duration
	now func() time.Time
}

// NewMinter returns a Minter over an Ed25519 private key. ttl <= 0 uses the
// default.
func NewMinter(key ed25519.PrivateKey, ttl time.Duration) *Minter {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Minter{key: key, ttl: ttl, now: time.Now}
}

// NewMinterFromSeed derives the Ed25519 signing key deterministically from seed.
// It is a convenience for single-process and test wiring, where one process
// holds both halves of the keypair; a split deployment supplies an explicit
// private key instead (docs/adr/0023).
func NewMinterFromSeed(seed []byte, ttl time.Duration) *Minter {
	priv, _ := KeyPairFromSeed(seed)
	return NewMinter(priv, ttl)
}

// Mint signs c into a token, stamping the issue and expiry times.
func (m *Minter) Mint(c Claims) (string, error) {
	if len(m.key) != ed25519.PrivateKeySize {
		return "", ErrInvalidToken
	}
	now := m.now()
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(m.ttl).Unix()
	// Every job token is an agent-plane token; default the audience so callers
	// need not set it (docs/adr/0032).
	if c.Audience == "" {
		c.Audience = AudienceAgent
	}

	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	b := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(m.key, []byte(b))
	return b + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Public returns the verification key matching this minter's private key.
func (m *Minter) Public() ed25519.PublicKey {
	return m.key.Public().(ed25519.PublicKey)
}

// TTL returns how long a minted token is valid, for an exchange response's
// expires_in (docs/adr/0024).
func (m *Minter) TTL() time.Duration {
	return m.ttl
}

// Verifier returns a Verifier for this minter's public key. It is a convenience
// for single-process and test wiring; a split deployment builds the Verifier
// from the separately distributed public key instead (docs/adr/0023).
func (m *Minter) Verifier() *Verifier {
	return NewVerifier(m.Public())
}

// Verifier validates job tokens with an Ed25519 public key. The web tier holds
// only this: it can authenticate a runtime's bearer token but cannot issue one.
type Verifier struct {
	key ed25519.PublicKey
	now func() time.Time
}

// NewVerifier returns a Verifier over an Ed25519 public key.
func NewVerifier(key ed25519.PublicKey) *Verifier {
	return &Verifier{key: key, now: time.Now}
}

// Verify checks the signature and expiry and returns the decoded claims.
func (v *Verifier) Verify(token string) (*Claims, error) {
	b, sigPart, ok := strings.Cut(token, ".")
	if !ok || b == "" || sigPart == "" {
		return nil, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil || len(v.key) != ed25519.PublicKeySize || !ed25519.Verify(v.key, []byte(b), sig) {
		return nil, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return nil, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrInvalidToken
	}
	if v.now().After(time.Unix(c.ExpiresAt, 0)) {
		return nil, ErrExpired
	}
	return &c, nil
}

// Validate implements authn.TokenValidator: it verifies a bearer token and maps
// its claims onto a workload Principal. The request is unused today; binding a
// token to request properties can be added without changing callers.
func (v *Verifier) Validate(_ *http.Request, token string) (*principal.Principal, error) {
	c, err := v.Verify(token)
	if err != nil {
		return nil, err
	}
	// The token must be audience-bound to the agent plane. A token minted for a
	// different audience must not authenticate a runtime request (docs/adr/0032).
	if c.Audience != AudienceAgent {
		return nil, ErrInvalidToken
	}
	return &principal.Principal{
		Kind:         principal.KindWorkload,
		Subject:      c.Subject,
		ActingUserID: c.ActingUserID,
		JobID:        c.JobID,
		Scopes:       c.Scopes,
	}, nil
}

// GenerateKeyPair returns a fresh random Ed25519 keypair, for an operator to
// provision the orchestrator's private key and the web tier's public key.
func GenerateKeyPair() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return priv, pub, nil
}

// KeyPairFromSeed deterministically derives an Ed25519 keypair from seed (hashed
// to the 32-byte seed Ed25519 expects). The single-process and test wiring use
// it so the in-process minter and verifier agree with no extra configuration; a
// split deployment provisions an independent keypair instead (docs/adr/0023).
func KeyPairFromSeed(seed []byte) (ed25519.PrivateKey, ed25519.PublicKey) {
	h := sha256.Sum256(seed)
	priv := ed25519.NewKeyFromSeed(h[:])
	return priv, priv.Public().(ed25519.PublicKey)
}
