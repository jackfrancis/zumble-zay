# ADR 0010: Agent runtimes are capability-scoped, not per-call

Status: Accepted — 2026-06-25

Builds on [0002](0002-agents-as-ephemeral-workloads.md) (agents are ephemeral
workloads), [0007](0007-orchestrator-colocated-until-spawn.md) (orchestrator
spawns runtimes), and [0009](0009-agent-runtime-contract-boundary.md) (the
runtime contract is the portability boundary).

## Context

Enrichment signals (`AwaitingMeSince`, `Participants`, `InboundRefs`, …) require
extra GitHub calls — O(items) fan-out, distinct from the single cheap search of
ingestion. The natural question for scale is the **granularity of a runtime**: do
we eventually make each GitHub API call its own runtime?

Per-call runtimes are the wrong unit. Spawn + token-mint + schedule overhead
dwarfs a sub-second API call; GitHub rate limits are **per-token/per-user**, so
fragmenting calls across runtimes does not multiply the budget and makes pacing
harder; and the efficient path (GraphQL) batches many calls into one request,
which per-call runtimes fight.

## Decision

The unit of a runtime is a **capability-scoped job over a batch within one
rate-limit (credential) domain** — not an individual API call.

- **Capabilities are distinct job types.** `github-ingest` (cheap, broad) and
  `github-enrich` (expensive, per-item) are separate `JobType`s, each with its
  own scopes, rate-limit budget, and failure domain, so one can be scaled,
  throttled, or circuit-broken without touching the other.
- **Scale by sharding instances**, not by atomizing calls: many runtimes of the
  same capability, each owning a shard (a user, or a batch of items), each doing
  **batched** calls within its own credential's rate-limit bucket.
- **Stages form a pipeline.** A successful `github-ingest` chains to
  `github-enrich` for the same user (the orchestrator enqueues it on success;
  enrichment does not chain further). Cheap ingestion gives a fast first paint;
  enrichment follows with the expensive signals.
- All capabilities still speak the one `ZZClient` contract (ADR 0009), so
  granularity choices never leak into ZZ core or the substrate.

## Consequences

- Adding a capability is an orchestrator `JobType` + `policies` entry + a
  launcher dispatch branch — the seams already exist. `github-enrich` is the
  first, landing `AwaitingMeSince` for review-requested PRs in-process today,
  shardable out-of-process later without ever atomizing calls.
- The naturally **long-lived** unit, if one emerges, is a per-`(user, provider)`
  token broker that paces all of that user's calls — the opposite of per-call.

## Known limitation / tracked follow-up

`github-enrich` currently **re-derives the full worklist** (a duplicate search)
rather than augmenting the already-persisted items, because the agent contract
has no read path and `worklist.Store.Upsert` replaces by ID. This is correct (no
data loss; merged reasons preserved) but wasteful. The planned improvement is an
**agent read contract** (a workload-scoped "fetch my shortlist") so enrichment
augments stored items in place instead of re-fetching — turning the pipeline
stages into true producer/refiner halves.

- *Realized:* the agent contract now has a read path — `GET /agent/worklist`
  (workload-scoped, no rescore/backfill) and `ZZClient.ListWorklist`. `github-
  enrich` reads the persisted items, augments review-requested PRs in place, and
  writes back only what changed — no duplicate search.
