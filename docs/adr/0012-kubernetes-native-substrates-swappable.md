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
   environment convention — `ZZ_BASE_URL`, `ZZ_JOB_TOKEN`, `ZZ_JOB_TYPE`,
   `ZZ_PROVIDER` (and optional `ZZ_GITHUB_BASE_URL`, `ZZ_ENRICH_LIMIT`) — so
   every launcher injects identically and the runtime is launcher-agnostic. The
   acting user and job id ride inside the signed token, not the environment. The
   encode (launcher: `agent.Env`) and decode (runtime: `agent.ParamsFromEnv`)
   halves live together so they cannot drift.
3. **The `Launcher` is selected at deploy time by configuration**
   (`LAUNCHER=inprocess|k8s-job|pod|kagent|kueue|sandbox|...`), and its interface
   is enriched to return a workload **handle** and surface **status/logs** plus
   accept substrate config — so swapping substrates and observing them is a
   config change, not a code change. The substrate-neutral abstraction lives in
   the `Launcher` **interface**; each concrete launcher is named for the
   substrate/resource it creates (`KubernetesJobLauncher`,
   `KubernetesPodLauncher`, `KagentLauncher`, `KueueLauncher`, `SandboxLauncher`),
   not a catch-all `WorkloadLauncher` — so per-substrate behaviour stays
   comparable rather than hidden behind one type with an internal switch.

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
5. **Reference `KubernetesJobLauncher`.** The first per-substrate launcher,
   named for the resource it creates (not as *the* Kubernetes launcher): a
   `batch/v1` Job — chosen for completion tracking, retry/backoff, and
   `ttlSecondsAfterFinished`, and as the Kueue-admissible unit — running the
   runtime image, token/URL injected, watched to completion; proven in the kind
   cluster.
6. **Config-driven launcher selection** (`LAUNCHER=...`) — the swap mechanism.
7. **Novel substrates as sibling launchers**, each named for its substrate and
   selectable by config — `KubernetesPodLauncher`, `KagentLauncher`,
   `KueueLauncher`, `SandboxLauncher`, custom — to compare lifecycle, isolation,
   identity, and cost. (Kueue's admission unit is a `Workload` CRD, so it is its
   own launcher rather than a reason to genericize the others.)

Steps 1-6 and the first sibling of step 7 are done; the swap **mechanism** is
complete and proven (in-process, `k8s-job`, and `k8s-pod` produce identical
pipeline output in kind). Remaining step-7 substrates are an additive backlog,
not unfinished plumbing:

- [x] `KubernetesJobLauncher` — reference (step 5).
- [x] `KubernetesPodLauncher` — bare Pod; lifecycle contrast (no retry/backoff/TTL GC).
- [ ] `KueueLauncher` — a suspended, queue-labelled Job admitted by Kueue against a
      ClusterQueue/LocalQueue; adds admission control/quota. Needs Kueue installed
      in the cluster + a dev queue.
- [ ] `SandboxLauncher` — agent-sandbox CRD/controller for stronger isolation.
      Needs that controller installed.
- [ ] `KagentLauncher` — a long-lived service, so it first requires ZZ to expose a
      public RFC 8693 token-exchange endpoint (a persistent service obtains a
      per-job token rather than being born with one; anticipated in ADR 0009).
