// Package mint issues and validates ZZ job tokens: short-lived, job-scoped
// bearer tokens carried by ephemeral agent runtimes (see docs/adr/0001 and
// docs/adr/0002). The token is self-describing — its claims encode the acting
// user, the granted scopes, and an expiry — so ZZ authorizes a request from the
// token alone.
//
// The token is a compact HMAC-SHA256-signed envelope (base64url(claims).sig),
// implemented with the standard library so ZZ takes on no JWT dependency while
// it both mints and validates. A move to asymmetric signing keys, so other
// parties can verify without the secret, is a later step.
package mint

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
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

// Claims are the self-describing contents of a job token. Mint sets IssuedAt
// and ExpiresAt; the caller supplies the rest.
type Claims struct {
	Subject      string            `json:"sub"`            // ephemeral runtime id
	ActingUserID string            `json:"auid"`           // user the runtime acts for
	Scopes       []principal.Scope `json:"scopes"`         // granted capabilities
	JobID        string            `json:"jid"`            // 1:1 with the spawned job
	Provider     string            `json:"prov,omitempty"` // provider the job targets
	IssuedAt     int64             `json:"iat"`
	ExpiresAt    int64             `json:"exp"`
}

// Minter mints and validates job tokens with a single HMAC key. The same
// instance is the orchestrator's minter and authn's TokenValidator.
type Minter struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewMinter returns a Minter. The signing key is domain-separated from the
// supplied secret so a job token can never be confused with another artifact
// (e.g. a session cookie) signed by the same secret. ttl <= 0 uses the default.
func NewMinter(secret []byte, ttl time.Duration) *Minter {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Minter{key: deriveKey(secret), ttl: ttl, now: time.Now}
}

// Mint signs c into a token, stamping the issue and expiry times.
func (m *Minter) Mint(c Claims) (string, error) {
	now := m.now()
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(m.ttl).Unix()

	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	b := base64.RawURLEncoding.EncodeToString(payload)
	return b + "." + m.sign(b), nil
}

// Verify checks the signature and expiry and returns the decoded claims.
func (m *Minter) Verify(token string) (*Claims, error) {
	b, sig, ok := strings.Cut(token, ".")
	if !ok || b == "" || sig == "" {
		return nil, ErrInvalidToken
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(m.sign(b))) != 1 {
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
	if m.now().After(time.Unix(c.ExpiresAt, 0)) {
		return nil, ErrExpired
	}
	return &c, nil
}

// Validate implements authn.TokenValidator: it verifies a bearer token and maps
// its claims onto a workload Principal. The request is unused today; binding a
// token to request properties can be added without changing callers.
func (m *Minter) Validate(_ *http.Request, token string) (*principal.Principal, error) {
	c, err := m.Verify(token)
	if err != nil {
		return nil, err
	}
	return &principal.Principal{
		Kind:         principal.KindWorkload,
		Subject:      c.Subject,
		ActingUserID: c.ActingUserID,
		Scopes:       c.Scopes,
	}, nil
}

func (m *Minter) sign(b string) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(b))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// deriveKey separates the job-token signing key from the raw secret so the same
// secret signing other artifacts cannot produce a colliding signature.
func deriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("zz/job-token/v1"))
	return mac.Sum(nil)
}
