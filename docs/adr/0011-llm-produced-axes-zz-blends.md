# ADR 0011: The four ranking axes are LLM-produced; ZZ blends and ratifies

Status: Accepted — 2026-06-26

Refines [0008](0008-worklist-ranking-signals-vs-scores.md) (signals vs scores)
and uses the capability model of [0010](0010-agent-runtimes-capability-scoped.md)
and the runtime contract of [0009](0009-agent-runtime-contract-boundary.md).

## Context

ADR 0008 defined four orthogonal axes — Relevance, Impact, Engagement, Urgency —
computed by a deterministic `Score()` from observed `Signals`. That deterministic
blend captures cheap structural facts (comment counts, review-requested, deadline
proximity) but **cannot express what the axes actually demand**: code blast
radius, correlation between participants and the project's top contributors,
movers-and-shakers vs passive onlookers, staleness that is really key people
being busy elsewhere, and dependencies implied by code overlap or design docs
rather than explicit links. Those are holistic, contextual judgments — LLM-shaped
problems, not formula-shaped ones.

## Decision

**The four axes are produced by an agentic LLM ("axis ranker"); ZZ remains the
deterministic blender and ratifier.**

1. The axis ranker emits, per item, each axis as a `0..1` value **with a
   rationale, cited evidence, and a confidence**. It is an agent capability (a
   new `JobType`), so ZZ core imports no LLM client (ADR 0006/0010).
2. **ZZ owns the blend:** `Rank` = weighted blend of the four axes (weights in
   configuration). ZZ also owns per-axis sorting and human override — the final
   ordering is deterministic *given the axes*.
3. The existing deterministic `Score()` is **repositioned, not discarded**: it
   becomes (a) the blend function (axes → rank) and (b) a **baseline/fallback**
   used when the LLM is unavailable or low-confidence, and as a **clamp
   reference** for ratification.
4. `Signals` are repositioned as the **evidence the LLM reasons over** and as the
   inputs to the deterministic fallback. The signal-collection pipeline
   (ingest/enrich) is unchanged.

## Axis definitions (crisp and mutually exclusive)

Tight definitions prevent the LLM from double-counting the same evidence across
axes.

- **Relevance — closeness to *you and your sphere*.** How connected the work is
  to *your* other active items (shared code, threads, or strategic effort) and
  whether the people driving/discussing it are the contributors *you* most work
  with or who drive *your* other work. Subjective to the user.
- **Impact — significance of the *work itself*, independent of you.** Breadth of
  code/feature/library surface affected, downstream-consumer disruption, and
  whether it changes fundamental upstream behavior. Objective; user-independent.
- **Engagement — *attention and who's paying it*.** How many are actively
  participating/watching and the **weight of those actors** (movers-and-shakers
  vs onlookers), including whether apparent staleness is real disengagement or
  the key people being busy/away elsewhere. About the crowd, not the artifact.
- **Urgency — *time pressure to act now*.** Whether it blocks other work
  (including dependencies inferred from code overlap and design docs, not just
  explicit links), deadline/freeze proximity, expressed anxiety in side channels
  (Teams/email/Slack), and fast-moving drivers who will merge soon. About *when*,
  not *how important*.

The disambiguating axis is: Relevance = relation to **me**; Impact = significance
in **the world**; Engagement = **who/how many** are watching; Urgency = **when**
I must act.

## Guardrails

Because `Rank` is a blend of the four axes, the LLM effectively controls
ordering — and upstream content is attacker-influenced. Therefore:

- **Evidence + confidence per axis** — no bare numbers; cite the signals/sources.
- **Fallback to the deterministic baseline** when the LLM is unavailable or a
  value is low-confidence.
- **Clamp** how far an axis may deviate from the baseline (a ratify step) to
  blunt prompt injection from issue/PR/comment text.
- **Top-K cap** (already built) — per-item LLM passes are expensive.
- **Eval harness** — non-deterministic axes cannot be unit-tested; regression
  against curated golden cases and flag LLM-vs-baseline divergence (precedent:
  [docs/DESIGN_REVIEW.md](../DESIGN_REVIEW.md)).

## Consequences

- Final ordering stays deterministic given the axes; blend weights are tunable;
  explainability holds at two levels (ZZ's blend, and the LLM's per-axis
  rationale).
- Axis production is non-deterministic, so the **eval harness replaces unit
  tests for axis quality**; the blend itself stays unit-tested.
- New `llm-rank` capability with an LLM credential in the runtime (env-provided
  first; vended by ZZ later, per ADR 0006).
- LLM-produced axes are agent-derived (`OriginAgent`); a human override
  (`OriginUser`) still wins.

## Build path

1. **Phase 1:** an `AxisRanker` interface with a deterministic stub, wired into
   an `llm-rank` capability that produces the four axes **from the signals we
   already store** (no new fetches); ZZ blends; deterministic score is the
   fallback and clamp. End-to-end green before a real model is attached.
2. **Phase 2:** real model behind the interface, then per-axis tool access —
   diffs for Impact, contributor graph for Relevance/Engagement, WorkIQ/M365 for
   Urgency and diffuse interest.
