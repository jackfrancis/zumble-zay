//go:build ray

package main

// Activate the optional Ray/KubeRay substrate: blank-importing it runs its
// launcher.Register so LAUNCHER=ray becomes selectable (docs/adr/0024, 0028).
// Gated behind the same build tag as the package, so the default orchestrator
// binary carries neither the registration nor KubeRay's client. A future
// out-of-tree provider would replace this with an import of a separate module —
// the seam (orchestrator.Launcher) is already the only contract.
import _ "github.com/jackfrancis/zumble-zay/internal/raylauncher"
