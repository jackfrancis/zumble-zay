// Package agentsandbox is an optional agent-runtime substrate that launches each
// job as a kubernetes-sigs/agent-sandbox Sandbox (agents.x-k8s.io/v1beta1, see
// docs/adr/0026): an isolated, single-pod workload (gVisor/Kata-capable) the
// agent-sandbox controller reconciles. The implementation is gated behind the
// "agent_sandbox" build tag, so the default build carries none of it; build with
// `-tags agent_sandbox` and select it with LAUNCHER=agent-sandbox.
package agentsandbox
