# ADR 0025: Callback-driven job completion

Status: Accepted — 2026-06-29

Builds on [0009](0009-agent-runtime-contract-boundary.md) (the runtime contract
is the substrate boundary) and [0024](0024-agent-runtime-portability.md) (async
dispatch, completion decoupled from the worker).

## Context

After ADR 0024 the orchestrator awaited an async job's completion by watching the
substrate (the k8s launcher polls the Job/Pod to a terminal state). That works
for Kubernetes, but it has two limits. A **fully detached** substrate — one the
orchestrator cannot watch (a runtime in another cluster, a serverless function,
a service it has no API access to) — has no watch signal at all. And even for
Kubernetes, the watch is a coarse ≤2s-poll backstop: it reports *the workload
terminated*, not *the runtime finished its work*, and it is slower than the
runtime simply saying so. ADR 0009 anticipated this: "completion can arrive two
ways — Launch returns, or the ingest callback lands — so job state must not be
hard-coupled to only the former."

## Decision

The runtime **reports terminal completion explicitly**, and the orchestrator
**races that callback against the launcher's watch**.

- **An optional third call in the runtime contract.** After vend and ingest
  (ADR 0009), a runtime may `POST /agent/complete {error?}` — same target (the
  web tier), same job-token bearer auth, one new path. The out-of-process runtime
  (`cmd/runtime`) sends it; the in-process launcher does not, because its
  completion is already the `Launch` return.
- **Routing reuses existing seams.** Runtime → web `/agent/complete` → the web
  tier forwards to the orchestrator through `controlplane.Client.Complete`
  (`Local` direct, or `HTTP` → `/control/complete`). No new network path, no new
  auth path, minimal contract expansion. *Considered and rejected:* runtime →
  orchestrator directly, which adds a runtime→orchestrator network path (plus
  RBAC/NetworkPolicy) and a separate job-token auth path on the orchestrator, for
  no benefit over the existing web hop.
- **Correlation rides the token.** The job id is a token claim, surfaced on the
  `Principal`, so a runtime can only complete *its own* job — never another's.
- **The orchestrator races.** It arms a per-job completion channel at dispatch;
  the async await selects between that callback and the launcher's `Await` (now
  the failure/timeout backstop) bounded by the per-job deadline. The first wins,
  `finish` runs exactly once, and the deferred context cancel unwinds the loser.
- **The Kubernetes launcher integrates with zero code change.** The orchestrator
  races *every* `AsyncLauncher`, so the existing watch simply stops being the
  primary signal and becomes the backstop.

## Consequences

- Happy-path completion is detected the instant the runtime reports — faster and
  more accurate than polling the workload phase — while the watch backstops a
  crashed runtime or a dropped report (the report is best-effort).
- A fully detached substrate is now expressible: dispatch, then rely on the
  callback with the per-job deadline as the only backstop. The in-cluster
  reference for this is the `k8s-pod-detached` launcher — the Pod launcher's
  dispatch with an await-the-deadline `Await` (no watch) — which needs only
  Pod-create RBAC, not get/list/watch. Its tradeoff: infra failures that never
  run the runtime (image pull, unschedulable, OOM-before-callback) surface only
  at the deadline, so it is an opt-in substrate, not the watched default.
- The runtime contract is now three calls (vend, ingest, complete); the third is
  optional and additive. A substrate author reimplements it as a single POST, or
  relies on the watch backstop and skips it.
- Best-effort delivery means no exactly-once guarantee; the race plus an
  idempotent `finish` (a single finalize per job, unknown/late job ids are no-ops)
  keep it correct under duplicate or late signals.
- In-process behaviour is unchanged: completion is the `Launch` return, and the
  callback is skipped.
