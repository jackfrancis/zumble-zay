# ADR 0008: Worklist ranking separates observed signals from computed scores

Status: Accepted — 2026-06-25

Refines [0004](0004-render-reads-persisted-zz.md) (the landing page reads
persisted ZZ metadata) by defining the shape of that metadata.

## Context

The worklist's value is synthesis: turning hundreds of GitHub items a user
could engage with into a short, ordered "what should I weigh in on today" list.
The original `Metadata` carried only computed scores (`Priority`, `Relevance`,
`Impact`, `Rank`) with nothing populating them, and the ingestion path discarded
the very facts a ranker needs — e.g. *why* an item surfaced (authored vs
assigned vs review-requested) was collapsed away during dedupe.

We want a ranking model that is:

- **Multi-dimensional.** "Lots of people are commenting," "relates to my active
  work," and "relates to trending tech" are different questions; blending them
  into one opaque number loses the lenses a maintainer actually wants.
- **Explainable.** A senior reviewer building consensus must be able to see
  *why* an item ranks where it does.
- **Re-scorable.** Weighting heuristics will change often; we must be able to
  re-rank historical items without re-fetching them from the provider.
- **Time-aware.** Heat and urgency decay; a score frozen at ingest goes stale.

## Decision

Separate **observed signals (facts)** from **computed scores (judgments)**.

1. **`Signals`** records measured facts about an item — relationship to the user
   (`Reasons`, `RelatedActive`), engagement (comments, participants, reactions,
   comment velocity/acceleration, influential actors, inbound references),
   temporal facts (opened/last-activity, an outstanding ask on the user, a
   deadline), strategic context (repo tier, labels, blocking count, roadmap
   themes), and soft/probabilistic external inputs (topics, trend score, WorkIQ
   diffuse interest). Signals carry an `ObservedAt` so freshness is auditable
   and so multiple producers (the GitHub agent, a WorkIQ agent) can write
   different fields on different cadences.

2. **`Metadata`** is ZZ's judgment derived from `Signals`, decomposed into four
   orthogonal axes — `Relevance` (closeness to me/my active work), `Impact`
   (strategic/org importance), `Engagement` (social heat: level + velocity), and
   `Urgency` (time pressure / someone blocked on me) — each normalized `0..1`
   and independently sortable. `Rank` is their weighted blend (the default
   "most important first"); `Priority` is a coarse human-facing band derived
   from `Rank`.

3. **Explainability is first-class.** `Metadata.Contributions` lists the signals
   that drove each axis with a signed weight and a human-readable detail, so the
   short `Rationale` string is derived, not authored.

4. **Origin is preserved.** Agent/system scoring is `OriginAgent`; an explicit
   human override is `OriginUser` and outranks it.

## Consequences

- The data structure lands now as an **additive, non-breaking** change (new
  fields and types only); existing sort keys and ingestion keep working with the
  axes zero-valued until a scorer populates them.
- Each axis becomes a first-class lens via the existing `?sort=` endpoint;
  `engagement` and `urgency` join `rank`/`priority`/`impact`/`relevance`/
  `updated`.
- **Time-dependent axes (`Engagement`, `Urgency`) must be recomputed at read or
  on a refresh tick, not frozen at ingest.** Computing `CommentAccel` and decay
  requires retaining a **prior `Signals` snapshot** per item, so the store gains
  a history obligation it did not have before.
- **Relatedness is a cross-item pass.** `RelatedActive` needs the user's whole
  active set in scope, so it runs after the per-item fetch, not inside it.
- Soft signals (`TrendScore`, `DiffuseInterest`) are probabilistic and arrive on
  a different cadence than GitHub facts; they are treated as inputs with their
  own freshness, never as ground truth.

## Open parameters (intentionally deferred, tunable without schema change)

- **Weights** per signal/axis live in configuration; start hand-set, later learn
  them from the user's actual dashboard engagement.
- **Normalization:** relative (min-max over the current set → always yields a
  ranked list) for the daily view vs absolute thresholds for alerting.
- **Decay half-lives** per axis and the **trend-window** length for velocity.
- **Snapshot retention** (last value vs a small ring) for velocity/acceleration.
