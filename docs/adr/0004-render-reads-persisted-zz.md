# ADR 0004: Landing page reads persisted ZZ data, not live GitHub

Status: Accepted — 2026-06-22

## Context

The landing page renders a well-ordered set of work. Ordering depends on
ZZ-specific metadata that cannot be inferred from the GitHub item alone. The
data could be fetched live from GitHub on each render, or read from ZZ's own
persisted, pre-enriched store.

## Decision

The render path reads persisted ZZ data only. GitHub/Graph retrieval is
**ingestion**: performed asynchronously by agent runtimes and written into ZZ.
The `GET /api/worklist` endpoint sorts server-side and returns the result. An
empty result (expected to be rare once persisted) triggers an idempotent
`Ingestor.EnsureBackfill` and returns `status: processing` so the UI can show a
waiting experience.

## Consequences

- Page latency is decoupled from GitHub API latency and rate limits.
- A single composed (BFF-style) endpoint keeps ordering logic server-side and
  off the client.
- Requires the persistence store and the ingestion flow (later increments);
  until then `Ingestor` is a logging no-op and the store is in-memory.
- Freshness becomes an ingestion concern (cadence, change detection), not a
  render concern.
