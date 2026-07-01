# ADR 0029: kagent as a durable BYO-A2A runtime substrate

Status: Accepted — 2026-07-01. Implemented in `internal/kagent` (the launcher +
a hand-rolled A2A JSON-RPC client), with the runtime side in `internal/agenta2a`
(an A2A server wrapping `agent.Run`) + `cmd/runtime-a2a`, and dev wiring in the
`Makefile` + `deploy/k8s/kagent/`. Validated live in kind end-to-end: ingest,
enrich, rank, and research dispatched through the kagent controller to a standing
BYO agent and completed via the ZZ callback; converse works given the
detached-completion and 15-minute-budget decisions below.

Builds on [0012](0012-kubernetes-native-substrates-swappable.md) (substrates are
swappable behind `orchestrator.Launcher`), [0024](0024-agent-runtime-portability.md)
(the launcher registry + async dispatch + token exchange), and
[0025](0025-callback-driven-completion.md) (detached, callback-driven
completion). It reuses the substrate-neutral runtime contract of
[0009](0009-agent-runtime-contract-boundary.md) and stands in deliberate tension
with [0002](0002-agents-as-ephemeral-workloads.md) (see Context).

## Context

[kagent](https://kagent.dev) is a CNCF Kubernetes-native framework for AI agents.
An agent is a custom resource (`kagent.dev/v1alpha2`) that its controller
reconciles into a **standing** Deployment + Service; invocation goes through the
controller, which fronts an [A2A](https://a2a-protocol.org) JSON-RPC endpoint
(`POST {controller}:8083/api/a2a/{ns}/{agent}/`, `message/send`) and proxies to
the agent's Service on `:8080`. The controller is an active intermediary: it
rewrites the JSON-RPC `id`, injects `configuration` defaults, and **persists task
history** (bundled Postgres in dev).

This makes kagent unlike every substrate ZZ has today in two ways.

- **It is the first *durable*-runtime substrate.** `inprocess`, `k8s-job`,
  `k8s-pod`, `k8s-pod-detached`, `agent-sandbox`, and `opensandbox` all create a
  fresh one-shot workload per job. kagent hosts a **long-lived** agent Deployment;
  the launcher *dispatches to* it rather than creating a workload. Like
  `opensandbox` (ADR 0027), ZZ is therefore a **client of a control plane** (the
  kagent controller over A2A), not a direct workload creator — so this path needs
  **no pod/job-create RBAC** on the orchestrator. Unlike `opensandbox`, the
  workload does not come and go per job; it stands.
- **It is in tension with ADR 0002** (agents are ephemeral spawned workloads).
  The resolution: ephemerality moves from the *process* to the *authorization*.
  The agent process is durable, but each dispatch still carries a fresh,
  job-scoped, short-TTL ZZ token minted per job (ADR 0023). ZZ neither creates nor
  owns the Deployment — the kagent controller does — so ZZ's ownership stays at
  the seam (the launcher + the token), not the pod lifecycle.

Three things had to be resolved before wiring it up.

1. **The BYO contract.** kagent can host a non-LLM **Bring-Your-Own** agent
   (`spec.type: BYO`): the controller owns a Deployment running our image, which
   need only serve `GET /.well-known/agent-card.json` (the readiness probe path)
   and A2A JSON-RPC at `/` on `:8080`. No ADK, no kagent SDK, no self-registration
   — a plain HTTP server qualifies. So the runtime must be *adapted* to A2A, not
   rewritten.
2. **Carrying a per-job credential.** A spike established that the controller
   **strips custom HTTP headers** (`Authorization`, `X-*`) before they reach the
   agent, but **forwards `params.message.metadata` intact**. Per-job data must
   therefore ride in A2A message metadata. Because the controller persists task
   history, that metadata can land in its Postgres store — so the token must be
   **short-TTL** (which aligns with ZZ's Ed25519 job tokens).
3. **A synchronous invocation cap.** The controller caps a *blocking*
   `message/send` at ~180s (proven live: a substantive converse failed at exactly
   180.000s with `context canceled` — the proxied connection was cut, not a ZZ
   timer). Any real converse or long rank would hit that wall.

## Decision

Ship kagent as an **untagged, self-registering substrate** that dispatches to a
durable BYO A2A agent, carries per-job params + token in message metadata, and
completes via the runtime callback.

- **Own package, own activation, no new module.** `internal/kagent` implements
  `orchestrator.Launcher` + `AsyncLauncher`, registers itself
  (`launcher.Register("kagent", …)`) from a package `init`, and is activated by
  its *own* blank-import file in `cmd/orchestrator`. The A2A client is a thin
  hand-rolled `net/http` JSON-RPC caller (no SDK), so `go.mod`/`go.sum` are
  untouched and **no build tag is needed** (cf. ADR 0027 — a tag isolates a heavy
  module, and there is none here). Endpoint + agent coordinates are read in
  `build()` from `KAGENT_ENDPOINT` / `KAGENT_AGENT_NAMESPACE` /
  `KAGENT_AGENT_NAME` (in-cluster FQDN defaults), so `internal/config` is
  untouched. Select with `LAUNCHER=kagent`.
- **Adapt `agent.Run` behind an A2A server; one decoder.** `internal/agenta2a`
  serves the agent card (readiness) + `message/send`, and `cmd/runtime-a2a` is the
  thin entrypoint. `paramsFromTask` builds a **metadata-first-then-env** lookup and
  calls the existing `agent.ParamsFromEnv` **verbatim**, so there is a single
  params decoder across every substrate and `AIToken` stays env-only. The
  runtime's job dispatch, credential vend, ingest, and completion logic are reused
  unchanged — kagent only changes *how the runtime is invoked*, not what it does.
- **Per-job in metadata, static on the Deployment.** The launcher puts only
  per-job keys in `message.metadata` (`ZZ_JOB_TYPE`, `ZZ_JOB_TOKEN`, and
  `ZZ_PROVIDER`/`ZZ_ITEM_ID` when set), via the shared `agent.Env*` constants.
  Static config (`ZZ_BASE_URL`, `ZZ_AI_*`) lives on the durable agent's Deployment
  env. Empty optional keys are **omitted**, not sent blank, because the
  metadata-first lookup would otherwise let an empty value shadow the Deployment
  env and fail validation.
- **Detached background run + `blocking:false` + callback completion (ADR 0025).**
  This is the resolution of the 180s cap. The A2A server sets
  `ReportCompletion=true`, runs `agent.Run` in a **background goroutine with its
  own** `context.WithTimeout(context.Background(), JobTimeout)` (not the request
  context, which dies when the response returns), and returns a non-terminal
  `submitted` task immediately. The client sends `configuration.blocking:false` so
  the controller acknowledges at once instead of waiting for a terminal task. The
  A2A task result is therefore **irrelevant to ZZ** — completion is 100% the
  orchestrator racing the runtime's `POST /agent/complete` callback against the
  launcher's deadline backstop. Accordingly the launcher is an `AsyncLauncher`:
  `Dispatch` is a non-blocking send that accepts unless the task comes back
  `failed`; `Await` waits the per-job deadline.
- **Converse and rank are one archetype on one standing agent; converse gets a
  15-minute budget.** Because the agent hosts the full `agent.Run` dispatch, every
  ZZ job type runs on the same Deployment. A substantive PR review (many tool
  calls over large files + slow synthesis) exceeds the 5-minute rank/research
  budget, so `github-converse` is given its **own** 15-minute deadline, kept in
  step across `agent.JobTimeout` and `orchestrator.deadlineFor`. (The deeper lever
  — trimming converse's context so reviews finish in ~5 minutes — is deferred to
  ADR 0015.)
- **Dev wiring keeps the AI token in its lane.** `LAUNCHER=kagent make dev-up`
  builds + loads the `runtime-a2a` image, `helm install`s the kagent CRDs +
  controller (with a **dummy** provider key — the BYO agent calls the model
  through ZZ's agentgateway, not a kagent `ModelConfig`, so no real key is
  needed), applies the BYO `Agent` CR, and rollout-restarts the standing agent so
  it adopts the reloaded `:dev` image. The agent runs in the `kagent` namespace and
  reaches the model via the agentgateway FQDN, so **no AI token crosses
  namespaces**; a bare `make dev-up` is byte-identical.

## Consequences

- **Default build untouched; references stay green; no seam change.** No new
  module, no build tag, no change to `Launcher`/`AsyncLauncher`, `ZZClient`, the
  `ZZ_*` contract, `runtimespec`, or the dispatch/completion path. The reference
  launchers (`inprocess`, `k8s-job`, `k8s-pod`, `k8s-pod-detached`) and
  `agent-sandbox`/`opensandbox` are unaffected — the cross-substrate
  non-regression invariant holds.
- **A new operational shape: a warm, standing runtime.** Unlike every other
  substrate, there is no per-job cold start and no workload to garbage-collect —
  but there *is* a persistent process the kagent controller owns and reconciles,
  and ZZ must rollout-restart it to adopt a new image. This is the accepted cost of
  putting a durable runtime behind the ephemeral-authorization model (see Context /
  ADR 0002).
- **The job token can reach kagent's task-history store.** Mitigated now by
  short-TTL tokens. The clean fix is the **token-exchange pull-path** (ADR 0024's
  `POST /control/token`): the runtime would exchange a job identifier for its token
  at spawn rather than receive it in metadata, keeping the token out of Postgres
  entirely. Deferred, not done.
- **The 180s cap is fully retired.** Because completion is callback-driven and the
  send is non-blocking, no ZZ job is bounded by the controller's synchronous proxy
  window — only by ZZ's own per-job deadline. A blocking, synchronous launcher was
  built first (Slice 2) and **deliberately superseded** by this detached design
  once the cap was observed live.
- **Activation prerequisites.** The kagent CRDs + controller installed in the
  cluster, the BYO `Agent` CR applied, the `runtime-a2a` image available to the
  cluster, and the agent's egress permitting it to reach ZZ's web tier (vend /
  ingest / the completion callback) and the AI endpoint. No orchestrator RBAC for
  pod/job creation is added on this path.
- **Merge-clean with concurrent substrate work (ADR 0028 ray/kuberay).** By
  staying in its own `internal/kagent` + `internal/agenta2a` packages and its own
  `cmd/orchestrator` blank-import file, reading its config in `build()`, adding no
  RBAC and no module, this substrate shares only trivial textual surfaces (this
  README row, the roadmap line, the `Makefile` `LAUNCHER` branch) with other
  in-flight substrates.
