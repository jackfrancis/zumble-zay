// Package substrate dispatches Zumble-Zay agent jobs to a durable Agent Substrate
// ("ate", github.com/agent-substrate/substrate) actor, instead of spawning an
// ephemeral workload per job like the in-cluster launchers (docs/adr/0035).
// Substrate multiplexes many actors onto a few warm worker pods and suspends /
// resumes an actor's whole process (RAM + filesystem) via gVisor
// checkpoint/restore, keeping the Kubernetes control plane off the request path.
//
// ZZ follows the durable-runtime archetype of docs/adr/0029 (kagent): the actor
// is a standing workload, provisioned out-of-band, that runs the exact
// cmd/runtime-a2a image (the ZZ runtime behind an A2A endpoint). This launcher is
// its orchestrator-side client. Dispatch is a plain net/http POST through the
// Substrate atenet-router with the actor's Host header; the router auto-resumes a
// suspended actor on the inbound traffic and proxies to it, so no gRPC is needed
// on the dispatch path. Per-job parameters and a single-use redemption ticket ride
// the A2A message metadata (the actor's env is frozen in its golden snapshot, so
// per-job values cannot ride env); static configuration lives on the ActorTemplate.
//
// It self-registers under LAUNCHER=substrate and is activated by a blank import in
// cmd/orchestrator. It pulls no third-party module (a hand-rolled net/http
// client), so it needs no build tag, and it reads its own SUBSTRATE_* env in
// build(), adding nothing to internal/config — merge-clean with other concurrent
// substrates (docs/adr/0024, 0027, 0029).
package substrate
