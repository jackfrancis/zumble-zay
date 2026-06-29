package launcher_test

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/launcher"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

func nullLog() *slog.Logger { return slog.New(slog.NewTextHandler(nopWriter{}, nil)) }

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestRegisterAndBuild(t *testing.T) {
	var built bool
	launcher.Register("test-build", func(*config.Config, *slog.Logger) (orchestrator.Launcher, error) {
		built = true
		return orchestrator.NoopLauncher{}, nil
	})

	l, err := launcher.Build(&config.Config{Launcher: "test-build"}, nullLog())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if l == nil || !built {
		t.Fatalf("factory was not invoked or returned nil (built=%v)", built)
	}
}

func TestBuildUnknownNameErrors(t *testing.T) {
	launcher.Register("test-known", func(*config.Config, *slog.Logger) (orchestrator.Launcher, error) {
		return orchestrator.NoopLauncher{}, nil
	})
	_, err := launcher.Build(&config.Config{Launcher: "test-nope"}, nullLog())
	if err == nil {
		t.Fatal("expected an error for an unregistered launcher")
	}
	// The error lists the registered names to aid diagnosis.
	if got := err.Error(); !contains(got, "test-known") {
		t.Fatalf("error should list registered substrates, got %q", got)
	}
}

func TestBuildPropagatesFactoryError(t *testing.T) {
	want := errors.New("no cluster")
	launcher.Register("test-err", func(*config.Config, *slog.Logger) (orchestrator.Launcher, error) {
		return nil, want
	})
	if _, err := launcher.Build(&config.Config{Launcher: "test-err"}, nullLog()); !errors.Is(err, want) {
		t.Fatalf("expected the factory error to propagate, got %v", err)
	}
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	launcher.Register("test-dup", func(*config.Config, *slog.Logger) (orchestrator.Launcher, error) {
		return orchestrator.NoopLauncher{}, nil
	})
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic on duplicate registration")
		}
	}()
	launcher.Register("test-dup", func(*config.Config, *slog.Logger) (orchestrator.Launcher, error) {
		return orchestrator.NoopLauncher{}, nil
	})
}

func TestRegisterRejectsEmptyNameAndNilFactory(t *testing.T) {
	mustPanic(t, "empty name", func() {
		launcher.Register("", func(*config.Config, *slog.Logger) (orchestrator.Launcher, error) {
			return orchestrator.NoopLauncher{}, nil
		})
	})
	mustPanic(t, "nil factory", func() { launcher.Register("test-nil", nil) })
}

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("expected a panic: %s", what)
		}
	}()
	fn()
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
