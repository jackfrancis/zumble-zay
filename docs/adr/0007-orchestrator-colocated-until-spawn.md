# ADR 0007: Orchestrator is co-located until it spawns real workloads

Status: Accepted — 2026-06-23

Refines [0002](0002-agents-as-ephemeral-workloads.md).

## Context

ADR 0002 names an orchestrator (durable identity) that spawns ephemeral
runtimes. Standing the orchestrator up as its own deployable — image, identity,
Kubernetes RBAC, mint client — is significant plumbing. For the first agentic
slice, runtimes are in-process goroutines, so the spawner holds no privileged
capability.

## Decision

Keep the orchestrator **logically separate** (its own package and identity) but
**physically co-located** inside the ZZ process for now. It obtains job tokens
through the **explicit mint step**, presenting an orchestrator identity, so the
RFC 8693 trust boundary exists in code even while the call is loopback.

**Extract** the orchestrator into its own runtime + identity when *either*:

- (a) it gains real workload-spawning privileges (Kubernetes Pod-creation RBAC,
  platform identity per [0003](0003-kubernetes-substrate.md)); **or**
- (b) ZZ scales past `replicas: 1` (a reconciler wants a single leader; the web
  tier wants horizontal scale — see [0005](0005-in-memory-first.md)).

## Consequences

- Fast path to an end-to-end agentic flow without K8s spawn machinery.
- No dangerous capability is added to the internet-facing process while runtimes
  are in-process (the spawner can only start goroutines).
- The extraction trigger is **explicit**, so co-location cannot silently harden
  into a design that couples a privileged spawner to the web tier.
- Requires discipline: the orchestrator must not shortcut the mint step, or
  extraction later becomes a redesign rather than a deployment change.
