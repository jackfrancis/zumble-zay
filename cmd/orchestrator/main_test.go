package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/launcher"
)

// The built-in launchers register themselves via this package's init (see
// main.go), so the registry resolves them here.
func TestBuiltinLaunchersRegistered(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("inprocess builds", func(t *testing.T) {
		l, err := launcher.Build(&config.Config{
			Launcher: "inprocess",
			Runtime:  config.RuntimeConfig{ZZBaseURL: "http://zumble-zay:8080"},
		}, log)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if l == nil {
			t.Fatal("expected a launcher, got nil")
		}
	})

	t.Run("unknown launcher fails fast", func(t *testing.T) {
		// Selection is the swap mechanism: a typo or an unregistered substrate
		// must be a startup error, never a silent fall back to in-process.
		if _, err := launcher.Build(&config.Config{Launcher: "k8sjob"}, log); err == nil {
			t.Fatal("expected an error for an unregistered launcher, got nil")
		}
	})
}
