# ADR 0026: agent-sandbox as an optional, build-tagged substrate

Status: Accepted — 2026-06-29

Builds on [0012](0012-kubernetes-native-substrates-swappable.md) (substrates are
swappable behind `orchestrator.Launcher`), [0024](0024-agent-runtime-portability.md)
(the launcher registry + async dispatch), and [0025](0025-callback-driven-completion.md)
(detached, callback-driven completion).

## Context

ADR 0012 step 7 named a `SandboxLauncher` for stronger isolation. The
[kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
`Sandbox` CRD (`agents.x-k8s.io/v1beta1`) is the natural fit: an isolated,
single-pod workload (gVisor/Kata-capable) reconciled by its own controller. The
payoff is **isolation** for running the agent runtime — increasingly relevant as
runtimes handle untrusted, LLM-influenced content (the converse tool-use surface,
future code execution; cf. ADR 0010) — not batch efficiency.

Two things had to be resolved before wiring it up.

1. **Which client.** The high-level Go SDK (`clients/go/sandbox`) is built for
   warm-pools + exec-into-sandbox and offers no way to set a custom `podTemplate`,
   so it does not fit "run *our* runtime image as a one-shot." The typed
   `api/v1beta1` types fit (the spec is `PodTemplate.Spec corev1.PodSpec`) and are
   lean (apimachinery-only), but they live in the agent-sandbox **root module**,
   so importing them adds that module to ZZ's `go.mod`/`go.sum` for the whole
   repo. A build tag isolates the *binary*, not the *module graph*.
2. **Completion.** A Sandbox is a long-running singleton with no batch-style
   completion status, so it cannot be "watched to done" like a Job.

## Decision

Ship agent-sandbox as a **first-class but optional** substrate, gated behind the
`agent_sandbox` build tag, that creates a **raw `Sandbox` CR via the client-go
dynamic client** and completes via the runtime callback.

- **Dynamic client, no new module.** The launcher builds the Sandbox as an
  `unstructured.Unstructured` (`apiVersion`/`kind`/`spec.podTemplate`/
  `spec.lifecycle`) and submits it with client-go's `dynamic` client — already a
  dependency. The **PodSpec is built typed** by the shared `internal/runtimespec`
  helpers and converted in, so the runtime container + `ZZ_*` injection contract
  are byte-identical to the Job/Pod launchers (the cross-substrate regression
  check). *Considered and rejected:* the typed `api/v1beta1` module — it drags the
  agent-sandbox module into everyone's `go.mod` (a build tag does not remove it
  from the graph) for marginal typing benefit on a five-field envelope.
- **Build-tag isolated, self-registering.** `internal/agentsandbox` registers
  itself (`launcher.Register("agent-sandbox", …)`) from a `//go:build
  agent_sandbox` file and is activated by a same-tagged blank import in
  `cmd/orchestrator`. The default build carries **none** of it — not the
  registration, not the substrate. Build with `-tags agent_sandbox` and select
  with `LAUNCHER=agent-sandbox`. (The tag is the Go-identifier form,
  `agent_sandbox`; everything user-facing is `agent-sandbox`.)
- **Detached completion + native self-reap.** A Sandbox has no batch completion,
  so the launcher is detached (ADR 0025): `Dispatch` creates the Sandbox, `Await`
  waits the per-job deadline, and the orchestrator races the runtime's
  `POST /agent/complete` callback. The Sandbox's own `spec.lifecycle.shutdownTime`
  + `shutdownPolicy: Delete` (stamped from the job deadline) self-reaps it — the
  Sandbox equivalent of a Job's TTL — so the launcher needs only `create` RBAC
  (`delete` is best-effort early cleanup).
- **Shared shape extracted to `internal/runtimespec`.** The runtime container /
  env / labels helpers moved out of `k8slauncher` into a substrate-neutral
  package, so `agentsandbox` reuses them **without importing a sibling launcher**
  — keeping it a clean, self-contained provider.

## Consequences

- **No default-build impact.** Default `go build`/`vet`/`test` exclude the package
  and add no dependency; the reference launchers (`inprocess`, `k8s-job`,
  `k8s-pod`, `k8s-pod-detached`) are untouched and stay green — the non-regression
  invariant holds.
- **Activation is opt-in and has prerequisites.** Running it requires: the
  orchestrator image built with `-tags agent_sandbox`, the agent-sandbox
  controller + CRDs installed in the cluster, `LAUNCHER=agent-sandbox`, the
  `sandboxes` RBAC rule, and the Sandbox's network policy permitting egress to the
  web tier (so the completion callback can land). For the kind dev cluster,
  `LAUNCHER=agent-sandbox make dev-up` wires all of it — the build tag (a `GO_TAGS`
  build-arg on the orchestrator image), the controller/CRD install, and the
  launcher selection (`kubectl set env`, which overrides the ConfigMap default);
  a bare `make dev-up` is byte-identical to before.
- **A clean path to an out-of-tree provider.** Because the substrate already
  depends only on the `orchestrator.Launcher` seam + `runtimespec`, splitting it
  into its own Go module later is bounded by one remaining step: promoting that
  seam out of `internal/` into a public package. Until then, in-tree + build tag
  gives binary isolation without that commitment.
