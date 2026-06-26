# ADR 0015: LLM axis proposals are authoritative (retire the ratify clamp and confidence floor)

Status: Accepted — 2026-06-26

Amends [0011](0011-llm-produced-axes-zz-blends.md): the four axes are still
LLM-produced, but ZZ no longer constrains a proposal against the deterministic
baseline.

## Context

ADR 0011 had the LLM *propose* the four axes and ZZ *ratify* them against a
signal-based baseline: a confidence floor (0.5) discarded low-confidence
proposals, and a deviation clamp (0.4) limited how far any axis could move from
baseline. The stated purpose was adversarial robustness — "attacker-influenced
content cannot fully hijack ordering" — plus calibration against a deterministic
anchor.

In practice that clamp is a quality compromise: it averages the LLM's
context-aware judgment back toward signal-counting, producing a result that is
neither a clean heuristic nor the model's actual conclusion. The product intent
is the opposite — the LLM, steered by English-language instructions, should be
**authoritative** for the dimensions it concludes on; tuning should happen in the
prompt, not by numeric averaging.

The adversarial justification is also weak for the current product: ZZ is a
single-user personal radar, and an item only surfaces because the user has a
real GitHub relationship to it (assignee/reviewer/author/mention). A malicious
actor crafting a PR title to rank higher on the user's *private* dashboard gains
little. So the clamp defends against a threat that barely exists today, at the
cost of the feature's core value.

## Decision

Retire the confidence floor and the deviation clamp. When an item carries an LLM
`AxisProposal`, ZZ uses the proposed axis values **directly, bounded only to
[0,1]**. The deterministic baseline is demoted to a pure **fallback**, used only
when no proposal is present (no model token, a model error, or the `StubRanker`
echoing the baseline). The `confidence` field is retained as information (and for
possible future use) but no longer gates anything.

ZZ still owns the **axis → Rank** combination (the fixed weights of ADR 0008);
the LLM is authoritative for the four axes it returns, not for how they blend.

## Consequences

- Ranking reflects the model's context-aware judgment faithfully; correctness is
  tuned in the model's instructions, not by tuning numeric guardrails.
- The deterministic path still works headless (no token → `StubRanker` →
  baseline), so the cluster-free tests and degraded mode are unchanged.
- **Known, deliberately deferred risk — discovery of non-participated work.**
  The radar will later ingest work the user is *not* already assigned to or
  participating in, for early *discovery* of relevant work from less-trusted
  sources. At that point the model reads attacker-controllable content it has no
  prior relationship to, and prompt-injection / rank-manipulation becomes a real
  vector that this decision removes the guardrail against. That must be addressed
  when discovery lands — e.g. a structural bound keyed off ZZ-verified signals
  (not model-judged content), provenance separation of trusted vs untrusted
  context, or a discovery-only clamp — rather than the blanket clamp removed
  here. Tracked, not solved.
- Supersedes the ratify mechanism of ADR 0011; the axis-production and
  blend-into-Rank parts of 0011 stand.
