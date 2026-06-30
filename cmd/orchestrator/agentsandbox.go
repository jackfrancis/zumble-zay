//go:build agent_sandbox

package main

// Activate the optional agent-sandbox substrate: blank-importing it runs its
// launcher.Register so LAUNCHER=agent-sandbox becomes selectable (docs/adr/0024,
// 0026). Gated behind the same build tag as the package, so the default
// orchestrator binary carries neither the registration nor the substrate's
// client. A future out-of-tree provider would replace this with an import of a
// separate module — the seam (orchestrator.Launcher) is already the only contract.
import _ "github.com/jackfrancis/zumble-zay/internal/agentsandbox"
