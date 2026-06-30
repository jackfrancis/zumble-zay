# ADR 0027: OpenSandbox as a remote-control-plane substrate

Status: Proposed — 2026-06-30. This record **reserves the number** and captures
the design direction; the full decision (and its validation) lands with the
implementation.

Builds on [0012](0012-kubernetes-native-substrates-swappable.md) (substrates are
swappable behind `orchestrator.Launcher`), [0024](0024-agent-runtime-portability.md)
(the launcher registry + async dispatch + token exchange),
[0025](0025-callback-driven-completion.md) (detached, callback-driven
completion), and [0026](0026-agent-sandbox-substrate.md) (an optional substrate as
a self-registering, build-isolated provider).

## Context

[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) is a
general-purpose sandbox platform for AI workloads: a lifecycle **server** reached
over HTTP (domain + API key) that provisions sandboxes on a Docker or Kubernetes
runtime, with strong isolation (gVisor / Kata / Firecracker), a credential vault,
and per-sandbox egress controls. It exposes a documented lifecycle/execution API
(OpenAPI) and SDKs.

Unlike every substrate ZZ has today, OpenSandbox is a **remote control plane**:
the orchestrator would be a *client* of the OpenSandbox server, which in turn
schedules the workload — it does not talk to the Kubernetes API directly the way
`k8s-job`, `k8s-pod`, and `agent-sandbox` do. It is being added **concurrently**
with a ray/kuberay substrate (ADR 0028); ADR 0024 already names a Ray cluster as
the canonical fully-detached substrate, and both ride the same detached +
callback completion path, so the seam and the concurrent-work conventions
(AGENTS.md) are the things to protect.

## Decision (proposed direction)

- **Own package, own activation.** A new `internal/opensandbox` implements
  `orchestrator.Launcher` + `AsyncLauncher`, registers itself
  (`launcher.Register("opensandbox", …)`) from a package `init`, and is activated
  by its *own* blank-import file in `cmd/orchestrator` — touching no existing
  launcher and no selection switch. Select with `LAUNCHER=opensandbox`.
- **Hand-rolled HTTP client, no new module.** Reach the OpenSandbox server with a
  thin `net/http` client against its OpenAPI rather than vendoring an SDK, so
  `go.mod`/`go.sum` are untouched and a build tag is *optional* (policy, not
  dependency-forced — cf. ADR 0026, where the tag isolates a binary, not the
  module graph). This keeps our footprint off the files the concurrent ray/kuberay
  work will edit.
- **Detached + callback completion (ADR 0025).** `Dispatch` creates the sandbox
  (sandbox id → `Handle.Ref`); `Await` waits the per-job deadline (optionally
  polling OpenSandbox's status API, which — unlike a raw Sandbox CR — it has); the
  orchestrator races the runtime's `POST /agent/complete`. The per-job deadline
  maps onto OpenSandbox's create-time `timeout` so a finished sandbox self-reaps
  (the Job-TTL analog); `kill` is best-effort early cleanup.
- **Reuse the substrate-neutral injection contract.** The `ZZ_*` environment
  comes from the shared `agent.Env` / `internal/runtimespec` source, so the
  runtime image and its contract are identical to every other substrate (the
  cross-substrate non-regression check). The ranking-model token can ride
  OpenSandbox's **Credential Vault** instead of a Kubernetes `secretKeyRef`.
- **No new ZZ RBAC; substrate-local config.** OpenSandbox's own identity
  schedules the workload, so the orchestrator needs no pod/job/sandbox-create
  RBAC for this path — only network egress to the OpenSandbox server and an API
  key. The endpoint + key are read in the launcher's `build()` (kept out of
  `internal/config`), so this substrate adds **zero** shared-config edits.

## Consequences

- **Default build untouched; references stay green.** No new module, no seam
  change; `inprocess`/`k8s-job`/`k8s-pod`/`k8s-pod-detached`/`agent-sandbox` are
  unaffected.
- **Activation prerequisites.** A reachable OpenSandbox server (domain + API
  key), the runtime image available to the OpenSandbox runtime, and an egress
  allowlist permitting the sandbox to reach ZZ's web tier (so vend/ingest and the
  completion callback land) plus the AI endpoint.
- **Merge-clean with ADR 0028 (ray/kuberay).** By staying in its own package +
  blank-import file, reading its config in `build()`, adding no RBAC, and adding
  no module, this substrate shares only trivial textual surfaces (the configmap
  `LAUNCHER` comment, this README row, the roadmap line) with the concurrent Ray
  work.
