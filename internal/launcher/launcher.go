// Package launcher is the registry of agent-runtime substrates (docs/adr/0024).
// A substrate registers a Factory under a name; the orchestrator builds the one
// named by configuration (the LAUNCHER env). New substrates — kagent,
// agent-sandbox, Ray, or anything implementing orchestrator.Launcher — register
// themselves from their own package's init and are activated by a blank import
// in cmd/orchestrator, so adding one touches no orchestrator wiring and no
// selection switch.
//
// This is the driver-registration pattern (cf. database/sql): the registry
// depends only on the orchestrator.Launcher seam and config, never on any
// substrate, so the substrate packages (and their client libraries) stay out of
// anything that merely selects a launcher.
package launcher

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/jackfrancis/zumble-zay/internal/config"
	"github.com/jackfrancis/zumble-zay/internal/orchestrator"
)

// Factory builds a launcher from configuration. It returns an error for an
// environment the substrate cannot run in (e.g. a Kubernetes launcher outside a
// cluster), so selection fails fast rather than silently degrading.
type Factory func(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a launcher factory under name. It panics on an empty name, a nil
// factory, or a duplicate — each is a registration bug, surfaced at startup
// rather than at job-dispatch time. Call it from a package init.
func Register(name string, f Factory) {
	if name == "" || f == nil {
		panic("launcher: Register requires a non-empty name and a factory")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("launcher: duplicate registration for " + name)
	}
	factories[name] = f
}

// Build constructs the launcher named by cfg.Launcher. An unknown name is a
// configuration error that lists the registered substrates: selection is the
// swap mechanism (docs/adr/0012), so a typo or an unregistered substrate must
// fail fast, never silently fall back.
func Build(cfg *config.Config, log *slog.Logger) (orchestrator.Launcher, error) {
	mu.RLock()
	f, ok := factories[cfg.Launcher]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown LAUNCHER %q (registered: %v)", cfg.Launcher, Names())
	}
	return f(cfg, log)
}

// Names returns the registered substrate names, sorted, for diagnostics and the
// unknown-launcher error.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
