// Package kagent dispatches Zumble-Zay agent jobs to a durable kagent Agent
// (kagent.dev) over the A2A protocol, instead of spawning an ephemeral workload
// per job like the in-cluster launchers (docs/adr/0024). kagent runs the ZZ
// runtime as a long-lived BYO Agent that speaks A2A (served by cmd/runtime-a2a);
// this launcher is its orchestrator-side client. Each job becomes one A2A
// message/send whose metadata carries the per-job parameters and the ZZ-minted
// job token, while the durable agent holds the static configuration (ZZ base URL,
// model endpoint/token) in its Deployment environment.
//
// It self-registers under LAUNCHER=kagent and is activated by a blank import in
// cmd/orchestrator. It pulls no third-party module (a hand-rolled net/http
// JSON-RPC client), so it needs no build tag, and it reads its own KAGENT_* env
// in build(), adding nothing to internal/config — merge-clean with other
// concurrent substrates (docs/adr/0024, 0027).
package kagent
