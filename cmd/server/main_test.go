package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/config"
)

func TestSelectLauncher(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("inprocess is the default substrate", func(t *testing.T) {
		l, err := selectLauncher(&config.Config{Launcher: "inprocess", Addr: ":8080"}, log)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if l == nil {
			t.Fatal("expected a launcher, got nil")
		}
	})

	t.Run("unknown launcher fails fast", func(t *testing.T) {
		// Selection is the swap mechanism: a typo or an unbuilt substrate must
		// be a startup error, never a silent fall back to in-process.
		if _, err := selectLauncher(&config.Config{Launcher: "k8sjob"}, log); err == nil {
			t.Fatal("expected an error for an unknown launcher, got nil")
		}
	})
}
