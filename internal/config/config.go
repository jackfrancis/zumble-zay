// Package config loads runtime configuration from the environment.
package config

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// Config holds all runtime configuration for the server.
type Config struct {
	Addr           string
	BaseURL        string
	AllowedOrigins []string
	SessionSecret  []byte
	CookieSecure   bool
	Providers      Providers
	// Launcher selects the agent-runtime substrate (e.g. "inprocess",
	// "k8s-job"). It is the swap mechanism for ADR 0012: changing substrates is
	// a config change, not a code change. The known set is validated where the
	// launcher is constructed, so an unknown value fails fast at startup.
	Launcher string
	// Runtime configures how out-of-process agent runtimes are launched. It is
	// consumed only by substrate launchers (e.g. the Kubernetes Job launcher);
	// the in-process launcher ignores it.
	Runtime RuntimeConfig
	// AI configures the chat-model ranker used by llm-rank jobs (docs/adr/0011).
	AI AIConfig
	// BotReviewers are GitHub logins whose review requests are automated rather
	// than a human explicitly asking — e.g. Prow's "k8s-ci-robot" auto-assigning
	// code owners. ZZ treats a review requested by one of these as a bare
	// assignment, not an explicit request, so it does not inflate the radar
	// (docs/adr/0015). Configurable via BOT_REVIEWERS (comma-separated); defaults
	// to k8s-ci-robot.
	BotReviewers []string
	// MintPrivateKey is the Ed25519 key that signs job tokens. Only the
	// orchestrator — the sole token issuer (docs/adr/0023) — needs it; it is nil
	// in a web tier configured with only a public key.
	MintPrivateKey ed25519.PrivateKey
	// MintPublicKey verifies job tokens. The web tier holds only this, so it can
	// authenticate a runtime's token but never mint one. It is always populated
	// (explicitly, or derived from SessionSecret for single-process runs).
	MintPublicKey ed25519.PublicKey
	// ControlPlaneURL is where the web tier reaches the orchestrator's control
	// API (docs/adr/0023). Empty means the orchestrator is co-located in this
	// process (the single-process/local default); set it to run the orchestrator
	// as a separate Deployment.
	ControlPlaneURL string
	// ControlPlaneAddr is the address the orchestrator's control API listens on.
	// It is consumed only by the orchestrator binary.
	ControlPlaneAddr string
	// ControlPlaneToken is the shared bearer the web tier presents to the
	// orchestrator's control API and the orchestrator checks. The control API is
	// cluster-internal, but it triggers privileged spawns, so it is authenticated.
	ControlPlaneToken []byte
}

// AIConfig configures the AxisRanker's chat model. The endpoint speaks the
// OpenAI-compatible chat-completions API, so any such provider works; empty
// endpoint/model fall back to the llm package defaults (GitHub Copilot + the
// default model). The token is a secret: the in-process path reads it directly,
// while the Kubernetes path injects it into the runtime via a Secret reference.
type AIConfig struct {
	Endpoint        string
	Model           string
	Token           string
	TokenSecretName string
	TokenSecretKey  string
}

// RuntimeConfig configures the out-of-process agent runtime: the image to run
// and where it reaches ZZ in-cluster (docs/adr/0012).
type RuntimeConfig struct {
	Namespace      string
	Image          string
	ZZBaseURL      string
	ServiceAccount string
}

// Providers holds the OAuth client credentials for each supported provider.
// A provider with an empty ClientID is treated as disabled.
type Providers struct {
	Google          OAuthApp
	GitHub          OAuthApp
	Microsoft       OAuthApp
	MicrosoftTenant string
}

// OAuthApp holds the credentials for a single OAuth application.
type OAuthApp struct {
	ClientID     string
	ClientSecret string
}

// Enabled reports whether the OAuth app has been configured.
func (a OAuthApp) Enabled() bool { return a.ClientID != "" && a.ClientSecret != "" }

// Load reads configuration from environment variables and validates it.
func Load() (*Config, error) {
	secret := os.Getenv("SESSION_SECRET")
	if len(secret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be set to at least 32 bytes")
	}

	mintPriv, mintPub, err := loadMintKeys([]byte(secret))
	if err != nil {
		return nil, err
	}

	// Review requests from these logins are automated, not explicit human asks;
	// default to Prow's bot so the common Kubernetes case works out of the box.
	botReviewers := splitAndTrim(os.Getenv("BOT_REVIEWERS"))
	if len(botReviewers) == 0 {
		botReviewers = []string{"k8s-ci-robot"}
	}

	cfg := &Config{
		Addr:           getEnv("ADDR", ":8080"),
		BaseURL:        strings.TrimRight(getEnv("BASE_URL", "http://localhost:8080"), "/"),
		AllowedOrigins: splitAndTrim(os.Getenv("ALLOWED_ORIGINS")),
		SessionSecret:  []byte(secret),
		CookieSecure:   getEnv("COOKIE_SECURE", "false") == "true",
		Providers: Providers{
			Google: OAuthApp{
				ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
				ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			},
			GitHub: OAuthApp{
				ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
				ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
			},
			Microsoft: OAuthApp{
				ClientID:     os.Getenv("MICROSOFT_CLIENT_ID"),
				ClientSecret: os.Getenv("MICROSOFT_CLIENT_SECRET"),
			},
			MicrosoftTenant: getEnv("MICROSOFT_TENANT", "common"),
		},
		Launcher: getEnv("LAUNCHER", "inprocess"),
		Runtime: RuntimeConfig{
			Namespace:      getEnv("RUNTIME_NAMESPACE", "zumble-zay"),
			Image:          getEnv("RUNTIME_IMAGE", "localhost/zumble-zay-runtime:dev"),
			ZZBaseURL:      getEnv("RUNTIME_ZZ_BASE_URL", "http://zumble-zay:8080"),
			ServiceAccount: os.Getenv("RUNTIME_SERVICE_ACCOUNT"),
		},
		BotReviewers: botReviewers,
		AI: AIConfig{
			Endpoint:        os.Getenv("AI_ENDPOINT"),
			Model:           os.Getenv("AI_MODEL"),
			Token:           os.Getenv("AI_TOKEN"),
			TokenSecretName: getEnv("AI_TOKEN_SECRET_NAME", "zumble-zay-secrets"),
			TokenSecretKey:  getEnv("AI_TOKEN_SECRET_KEY", "AI_TOKEN"),
		},
		MintPrivateKey:    mintPriv,
		MintPublicKey:     mintPub,
		ControlPlaneURL:   strings.TrimRight(getEnv("CONTROL_PLANE_URL", ""), "/"),
		ControlPlaneAddr:  getEnv("CONTROL_PLANE_ADDR", ":8090"),
		ControlPlaneToken: []byte(os.Getenv("CONTROL_PLANE_TOKEN")),
	}

	return cfg, nil
}

// loadMintKeys resolves the Ed25519 job-token keypair (docs/adr/0023). An
// explicit MINT_PRIVATE_KEY makes this process an issuer (orchestrator); an
// explicit MINT_PUBLIC_KEY makes it verify-only (web tier) with no private key.
// With neither set, both halves are derived from the session secret so a
// single-process run works with no extra configuration — the split deployment
// sets explicit keys, and the verify-only web tier never reaches this branch to
// re-derive the private key.
func loadMintKeys(seed []byte) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	if s := os.Getenv("MINT_PRIVATE_KEY"); s != "" {
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, nil, fmt.Errorf("MINT_PRIVATE_KEY must be a base64-encoded %d-byte Ed25519 private key", ed25519.PrivateKeySize)
		}
		priv := ed25519.PrivateKey(raw)
		return priv, priv.Public().(ed25519.PublicKey), nil
	}
	if s := os.Getenv("MINT_PUBLIC_KEY"); s != "" {
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, nil, fmt.Errorf("MINT_PUBLIC_KEY must be a base64-encoded %d-byte Ed25519 public key", ed25519.PublicKeySize)
		}
		return nil, ed25519.PublicKey(raw), nil
	}
	h := sha256.Sum256(seed)
	priv := ed25519.NewKeyFromSeed(h[:])
	return priv, priv.Public().(ed25519.PublicKey), nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
