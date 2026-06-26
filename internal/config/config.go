// Package config loads runtime configuration from the environment.
package config

import (
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
	}

	return cfg, nil
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
