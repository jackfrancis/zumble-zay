# ADR 0027: OpenSandbox as a remote-control-plane substrate

Status: Accepted — 2026-06-30. Implemented in `internal/opensandbox` as a
create-keepalive + exec launcher over a hand-rolled lifecycle/execd client (with
tests); not yet validated against a live OpenSandbox server.

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

A second thing the implementation had to resolve: OpenSandbox sandboxes are
**long-lived, exec-into** environments (an `execd` agent, a keep-alive default
entrypoint, readiness = `execd /ping`), not one-shot workloads. ZZ's runtime is a
one-shot that runs, calls back, and exits. Running it as the container entrypoint
fights the platform — it would loop under `restartPolicy: Always`, or read as
`Terminated`/`Failed` when it exits — so the launcher uses **create-keepalive +
exec** instead.

## Decision

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
- **Create keep-alive, then exec; detached completion (ADR 0025).** `Dispatch`
  creates a keep-alive sandbox (`tail -f /dev/null`, sandbox id → `Handle.Ref`),
  polls the lifecycle API until it is `Running`, resolves its `execd` endpoint, and
  execs `/runtime` into it as a background command. The exec runs in `Dispatch`
  (not `Await`) because only `Dispatch` receives the spec + token the runtime
  needs. `Await` then waits the per-job deadline (the backstop) and best-effort
  deletes the sandbox; the orchestrator races the runtime's `POST /agent/complete`.
  The per-job deadline maps onto OpenSandbox's create-time `timeout` so a finished
  sandbox self-reaps (the Job-TTL analog).
- **Reuse the substrate-neutral injection contract — via the exec env.** The
  `ZZ_*` map comes from the shared `runtimespec.Env` source, identical to every
  other substrate (the cross-substrate non-regression check), but is delivered as
  the **execd command environment** rather than container env (the container is a
  generic keep-alive). The ranking-model token rides as a value today (OpenSandbox
  is remote, so the in-cluster `secretKeyRef` does not apply); OpenSandbox's
  **Credential Vault** is the documented hardening.
- **No new ZZ RBAC; substrate-local config.** OpenSandbox's own identity
  schedules the workload, so the orchestrator needs no pod/job/sandbox-create
  RBAC for this path — only network egress to the OpenSandbox server and an API
  key. The endpoint + key are read in the launcher's `build()` (kept out of
  `internal/config`), so this substrate adds **zero** shared-config edits.

## Consequences

- **Default build untouched; references stay green.** No new module, no seam
  change; `inprocess`/`k8s-job`/`k8s-pod`/`k8s-pod-detached`/`agent-sandbox` are
  unaffected.
- **A shell-bearing runtime image is required.** OpenSandbox runs the command via
  `sh -c` inside the sandbox, and the keep-alive entrypoint is `tail -f /dev/null`,
  so the distroless ZZ runtime image (no shell, no `tail`) will not run here. A
  dedicated image carrying a shell + `/runtime` is needed, selected with
  `OPENSANDBOX_RUNTIME_IMAGE`.
- **`Dispatch` is no longer just a create.** Because the exec needs the spec +
  token (which `Await` does not receive), provisioning + exec happen on the
  dispatch worker, so a slow sandbox start holds a worker — a deliberate trade-off
  of this substrate, not a regression of the shared seam.
- **Activation prerequisites.** A reachable OpenSandbox server (domain + API
  key), the shell-bearing runtime image available to the OpenSandbox runtime, and
  an egress allowlist permitting the sandbox to reach ZZ's web tier (so vend/ingest
  and the completion callback land) plus the AI endpoint.
- **Dev (kind): `LAUNCHER=opensandbox make dev-up`.** Experimental and additive
  (a bare `make dev-up` is byte-identical): it builds + loads the shell-bearing
  runtime image (`Dockerfile.runtime-shell`), `helm install`s the controller from
  its **published chart release** (self-consistent with its image — the source
  chart on a moving ref drifts ahead and passes flags the published binary
  rejects) and the lifecycle server from a pinned source checkout (Kubernetes
  runtime, `batchsandbox` provider, a dev API key, the charts' default published
  images), and sets the orchestrator's `OPENSANDBOX_*` env plus a cross-namespace
  `RUNTIME_ZZ_BASE_URL` (sandboxes run in the OpenSandbox namespace, not the web
  tier's). `helm`/`git` are required; `AI_TOKEN` is left to the user (no Secret
  reference), so ranking falls back to the stub until set. Not yet validated
  end-to-end — expect to tune image pulls and the batchsandbox template per cluster.
- **Merge-clean with ADR 0028 (ray/kuberay).** By staying in its own package +
  blank-import file, reading its config in `build()`, adding no RBAC, and adding
  no module, this substrate shares only trivial textual surfaces (the configmap
  `LAUNCHER` comment, this README row, the roadmap line) with the concurrent Ray
  work.
