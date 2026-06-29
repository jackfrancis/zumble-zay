# ADR 0022: Discussion-derived "research" re-weighting of the ranking axes

Status: Accepted — 2026-06-28

Extends [0011](0011-llm-produced-axes-zz-blends.md)/[0015](0015-llm-axes-authoritative.md)
(the four axes; LLM authoritative; prompt-steered, no numeric clamps),
[0018](0018-per-item-assistive-conversation.md)/[0019](0019-conversation-as-ephemeral-agent.md)/[0020](0020-conversation-github-tool-use.md)
(the per-item conversation and its tool-grounded evidence), and
[0002](0002-agents-as-ephemeral-workloads.md)/[0007](0007-orchestrator-colocated-until-spawn.md)/[0012](0012-kubernetes-native-substrates-swappable.md)
(per-item ephemeral agents; reconcilers).

## Context

ZZ ranks work items on four axes (relevance, impact, engagement, urgency) from
GitHub metadata. The per-item conversation (ADR 0018–0020) now produces
**tool-grounded evidence** about an item that the raw metadata cannot encode —
e.g. that a CVE backport's urgency is overstated because upstream itself declined
to backport it, or that a "redundant-looking" PR is actually still needed. We
want that evidence to inform the ranking.

Two constraints shape the design:

1. **GitHub metadata stays authoritative — but it is not perfect.** The goal is a
   *virtuous feedback loop*: the LLM does fact-based analysis, that analysis
   guides project administrators to fix the GitHub metadata itself (comments,
   labels, closing stale issues), and over a long-lived item the conversation and
   the metadata **converge**. While the metadata is still wrong, the discussion
   must have **teeth** — it must be able to move the ranking, not merely annotate.
2. **Users must keep treating the metadata seriously**, not the tool as a source
   of truth. The discussion may not become a priority-inflation lever: chatting
   "make this urgent" must move nothing; only *evidence* may move the rank.

## Decision

Add a **"research" layer**: a discussion-derived re-weighting applied on top of
the metadata-based foundation, computed by a dedicated per-item agent, scheduled
by staleness.

### 1. The research layer: per-axis multipliers

The conversation produces **four multipliers**, one per foundation axis:

```
adjusted_axis = clamp( foundation_axis × research_multiplier , 0, 1 )
rank          = existing_weighted_blend(adjusted_axes)        // weights unchanged
```

- **1.0 = neutral** (no change); range **[0.0, 2.0]**; default **1.0**.
- **No thread ⇒ all 1.0** — the common case is exactly unaffected, so the
  foundation stands alone and "metadata seriously" is preserved by construction.
- **Prompt-soft, evidence-gated:** the model returns 1.0 unless the conversation
  supplies *evidence* (verified facts, decisions, confirmed/refuted blockers)
  that materially changes the picture — never on assertion, sentiment, or a
  request to be prioritized. The thread is treated as untrusted (ADR 0020).
- **Per-axis, not a scalar:** good research moves axes *differently* (see the
  worked example: urgency down hard, impact down slightly, relevance flat). A
  single scalar would smear those together.
- The multipliers are **self-documenting provenance**: the four numbers *are* the
  explanation of how research moved the rank, alongside a short rationale.

Why multiplicative: it is inherently anchored. A near-zero foundation stays
near-zero under any multiplier (you cannot manufacture relevance for an item you
have no relationship to), and evidence can move an axis either direction.

### 2. Serialized two-pass: foundation then research

- **Pass 1 — foundation (unchanged):** the existing batch `llm-rank` scores the
  GitHub metadata → the proposal cached in `Signals.Proposed`. This is the anchor.
- **Pass 2 — research (new, per item, only when a thread exists):** a dedicated
  agent reads the cached foundation + the thread and emits the multiplier vector.

`Score` consumes both: `final = clamp(foundation × research)`, then blends as today.

### 3. The research agent (per work unit)

A per-item agent — a near-clone of `github-converse` (ADR 0019):

- A new job type `github-research`, per item (its `JobSpec` carries `ItemID`),
  one workload spawned **per work unit**. Many small agents is the intended scale
  symptom (ADR 0002/0012), not a problem to design away.
- **No provider credential in v1.** It reasons over data ZZ already holds — the
  cached foundation proposal and the stored thread — and writes back the
  multipliers. So it is *lower* privilege than converse: `provider=""`, scopes
  `[signals:read, metadata:write]`, no vended GitHub token. (Live re-verification
  is a future additive credential, not v1.)

### 4. Staleness reconciler (the trigger)

A re-rank is needed when the ranking is older than its freshest input. The
timestamps already exist:

- `Metadata.ScoredAt` — when the item was last ranked.
- `GitHub.UpdatedAt` / `Signals.LastActivityAt` — GitHub freshness (set at ingest).
- `Message.At` — every discussion entry.
- `Signals.Proposed` — the cached foundation, so pass 2 can run standalone.

Rule, and the input that fired it selects the pass:

> rank-stale when `ScoredAt < max(GitHub.UpdatedAt, lastMessage.At)`

- `GitHub.UpdatedAt > ScoredAt` → foundation changed → re-run pass 1 (+ pass 2 if
  a thread exists).
- `lastMessage.At > ScoredAt`, GitHub unchanged → only the discussion moved →
  re-run **pass 2 only**, reusing the cached foundation (one LLM call, no GitHub).

The trigger is **not** per-entry; an aggressive reconcile tick (~3 min) scans
items, finds the stale ones, and enqueues per-item research jobs. The
orchestrator already dedups per item+type, so a burst of discussion collapses to
one job per item. It is a textbook reconciler (same ticker pattern as the session
cleaner); per ADR 0007 it is leader-gated once ZZ passes `replicas: 1` (trivial
at 1 today).

### 5. Data model

`ResearchAdjustment{Relevance, Impact, Engagement, Urgency float64, Rationale
string, AppliedAt time.Time}` is a scoring **input** (stored at `Signals.Research`,
beside `Signals.Proposed`). It is preserved across re-ingest like the thread and
hidden state (it is discussion-derived, never re-fetched from the provider). The
foundation (`Signals.Proposed`) and the research (`Signals.Research`) remain
separately inspectable, which powers both the feedback loop and the UI.

### 6. The feedback loop is observable

The multiplier vector *is a measurement of how wrong the metadata currently is.*
A sustained urgency ×0.5 means "the metadata overstates this by 2×." Surfacing
the multipliers + rationale tells an administrator exactly what to fix in GitHub;
when they fix it, the **foundation** becomes correct and research relaxes toward
1.0. `research → 1.0` is convergence, made visible.

## Worked example (conversation.txt: PR #9518, CVE backport to release-1.35)

Foundation: assignee + security/CVE + release-branch backport + `needs-rebase` ⇒
high relevance/impact/urgency, moderate engagement. The thread's tool-grounded
finding — core kube `release-1.34`/`1.35` themselves did not backport the grpc
fix and closed the tracking issue; the otel CVE is low-severity — yields:

| Axis | Foundation | research × | Adjusted | Why |
| --- | --- | --- | --- | --- |
| relevance | ~0.85 | 1.0 | ~0.85 | Still the user's assignment; research clarifies the decision, not whose it is. |
| impact | ~0.80 | 0.9 | ~0.72 | grpc (CVSS 9.1) severity unchanged; only the otel piece is low-severity → a gentle trim. |
| engagement | ~0.50 | 1.0 | ~0.50 | No evidence the measured heat misleads either way. |
| urgency | ~0.80 | 0.5 | ~0.40 | Upstream release branches declined the backport → the CVE-label urgency is materially overstated. |

Net: the item slides from "high-urgency, act now" to "still yours to decide,
defensible not to rush" — exactly the thread's conclusion, reproduced mechanically,
while relevance keeps it on the radar.

## Consequences

- The discussion has **teeth** (it moves the final `Rank`), but stays anchored
  (multiplicative on the foundation), evidence-gated (no inflation by assertion;
  untrusted thread), and self-documenting (the multipliers explain themselves).
- **Scale:** potentially many per-item research agents — the intended stress test
  of agentic-on-Kubernetes, not a defect.
- **Cost:** an extra LLM call per stale threaded item, bounded by the reconcile
  timer, per-item dedup, and the only-when-a-thread-exists gate; pass-2-only
  re-runs avoid a redundant foundation call when just the chat changed.
- **Alignment with ADR 0015:** still prompt-steered and LLM-authoritative — the
  only bounds are the multiplier range [0,2] and the axis range [0,1], not a
  deviation clamp toward a baseline.
- **Re-ingest:** `Signals.Research` is preserved across re-ingest (like the
  thread); agents never re-derive it from the provider.
- **Open / deferred:** blend weights remain hand-set (ADR 0008); how research
  confidence factors in; and live re-verification (an additive credential) — all
  out of scope here.

## Build order

1. `ResearchAdjustment` data + the `Score` blend + re-ingest preservation + the
   `ResearchRanker` seam and its LLM implementation + the research prompt.
2. The per-item `github-research` agent (job type, runtime, orchestrator entry,
   composition-root wiring).
3. The staleness reconciler (ticker, the rule above, per-item enqueue).
