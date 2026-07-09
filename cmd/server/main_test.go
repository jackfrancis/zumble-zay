package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/mint"
)

func TestBuildControlClient(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("remote when a control-plane URL is set", func(t *testing.T) {
		// The web tier authenticates with its projected ServiceAccount token, so a
		// remote control plane requires its path (docs/adr/0034). The file is read
		// per request, so it need not exist to construct the client.
		cp, stop, err := buildControlClient(&config.Config{
			ControlPlaneURL:       "http://orchestrator:8090",
			ControlPlaneTokenPath: "/var/run/secrets/tokens/control-plane-token",
		}, log)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer stop()
		if cp == nil {
			t.Fatal("expected a control client, got nil")
		}
	})

	t.Run("remote mode fails fast without a projected token path", func(t *testing.T) {
		// No shared-secret fallback: without CONTROL_PLANE_TOKEN_PATH the web tier
		// has no identity to present, so it must not build a client (docs/adr/0034).
		if _, _, err := buildControlClient(&config.Config{ControlPlaneURL: "http://orchestrator:8090"}, log); err == nil {
			t.Fatal("expected an error without CONTROL_PLANE_TOKEN_PATH, got nil")
		}
	})

	t.Run("co-located orchestrator with a signing key", func(t *testing.T) {
		priv, _, err := mint.GenerateKeyPair()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		cp, stop, err := buildControlClient(&config.Config{Addr: ":8080", MintPrivateKey: priv}, log)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer stop()
		if cp == nil {
			t.Fatal("expected a control client, got nil")
		}
	})

	t.Run("co-located mode fails fast without a signing key", func(t *testing.T) {
		// A verify-only configuration (no private key) cannot host the
		// co-located orchestrator: it is the sole token issuer (docs/adr/0023).
		if _, _, err := buildControlClient(&config.Config{Addr: ":8080"}, log); err == nil {
			t.Fatal("expected an error without a signing key, got nil")
		}
	})
}
