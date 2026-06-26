# ADR 0012: Kubernetes-native agent substrates are swappable behind a runtime artifact

Status: Accepted — 2026-06-26

Makes concrete the substrate seam of [0009](0009-agent-runtime-contract-boundary.md)
and the capability model of [0010](0010-agent-runtimes-capability-scoped.md);
advances [0007](0007-orchestrator-colocated-until-spawn.md) (orchestrator
co-located until it spawns real workloads) and [0002](0002-agents-as-ephemeral-workloads.md).

## Context

A primary purpose of this project is to **stress-test agentic application
building on Kubernetes** — to swap in and out novel agentic infrastructure
(kagent, agent-sandbox, raw Jobs, custom controllers) and compare their
lifecycle, isolation, identity, and cost characteristics. So Kubernetes-native
agent infrastructure is a **first-class concern**, not a scaling afterthought.

The conceptual foundations are already in place and correctly shaped:

- `orchestrator.Launcher` is the dispatch seam.
- `agent.ZZClient` is a substrate- and language-neutral runtime ABI (vend
  credential → list worklist → ingest), so any runtime that speaks it is
  swappable (ADR 0009).
- Per-job ZZ-minted scoped tokens exist.
- A kind-based testbed exists (`make dev-up`).

But the machinery to run a runtime **out of process** does not. Today
`agent.Run/RunEnrich/RunRank` are Go functions invoked inline by
`InProcessLauncher`. The blocking gaps:

1. **The runtime is not a deployable artifact.** No `cmd` entrypoint, no
   container image — a Kubernetes launcher would have nothing to launch.
2. **The `Launcher` contract is too thin** — returns only `error`; no handle,
   status/logs, or substrate config (image, namespace, CR kind).
3. **No injection contract** for delivering the token + ZZ base URL + job spec to
   an out-of-process runtime.
4. **The orchestrator has no Kubernetes identity or API client** (RBAC, SA).
5. **No config-driven launcher selection** — the launcher is hardcoded.
6. **No workload↔job-state reconciliation** — with Pods/CRs the status source of
   truth is Kubernetes, not the orchestrator's in-memory record.

## Decision

Make substrates **swappable by configuration** behind three stable artifacts:

1. **One runtime artifact.** Extract a single runtime entrypoint that dispatches
   on `JobType` and speaks only `ZZClient`, packaged as `cmd/runtime` + a
   container image. `InProcessLauncher` drives the *same* entrypoint in-process,
   proving "one runtime, many launchers."
2. **A runtime injection contract.** A runtime receives everything via a fixed
   environment convention — `ZZ_BASE_URL`, `ZZ_JOB_TOKEN`, and the job spec
   (`ZZ_JOB_TYPE`, `ZZ_PROVIDER`, `ZZ_ACTING_USER`, `ZZ_JOB_ID`) — so every
   launcher injects identically and the runtime is launcher-agnostic.
3. **The `Launcher` is selected at deploy time by configuration**
   (`LAUNCHER=inprocess|k8s-job|kagent|sandbox|...`), and its interface is
   enriched to return a workload **handle** and surface **status/logs** plus
   accept substrate config — so swapping substrates and observing them is a
   config change, not a code change.

ZZ core continues to import no provider/model client and no Kubernetes client;
the Kubernetes API client lives behind a `Launcher` implementation, keeping the
core substrate-agnostic.

## Consequences

- Substrate swapping becomes **trivial and additive**: the runtime logic and ZZ
  core never change; a new substrate is a new `Launcher` implementation plus a
  config value. Different runtimes may even be written in other languages, since
  the contract is HTTP + token.
- The orchestrator gains a **Kubernetes identity** once it spawns real workloads
  (a ServiceAccount + RBAC to create Jobs/Pods/CRs), realizing ADR 0007's
  deferred step; it may later be extracted into its own runtime.
- The injection contract assumes the runtime has **network egress to ZZ**
  (ADR 0009); fully isolated sandboxes either allow egress or invert to a push
  model.
- Long-lived service substrates (kagent) need a **public token-exchange
  endpoint** (RFC 8693) so a persistent service obtains a per-job token rather
  than being born with one (anticipated in ADR 0009).
- Lifecycle observability (workload↔job reconciliation) becomes part of the
  orchestrator so substrates can be compared.

## Build path (resumable)

Each step is additive and independently shippable; none touches ZZ core.

1. **Runtime artifact.** Single `Run(spec)` dispatching on `JobType`; add
   `cmd/runtime` + image; reframe `InProcessLauncher` to drive it. No behavior
   change.
2. **Injection contract.** Define and document the `ZZ_*` env convention; the
   runtime reads it; `InProcessLauncher` passes it in-memory.
3. **Launcher contract v2.** Return a handle + status; accept substrate config;
   keep `InProcessLauncher` conformant.
4. **Orchestrator Kubernetes identity.** client-go + ServiceAccount + RBAC
   (create Jobs in a namespace), behind the `Launcher` seam.
5. **Reference `KubernetesJobLauncher`.** A `batch/v1` Job running the runtime
   image, token/URL injected, watched to completion; proven in the kind cluster.
6. **Config-driven launcher selection** (`LAUNCHER=...`) — the swap mechanism.
7. **Novel substrates** (kagent, agent-sandbox, custom) as additional
   `Launcher` implementations, each selectable by config; compare lifecycle,
   isolation, identity, and cost.
