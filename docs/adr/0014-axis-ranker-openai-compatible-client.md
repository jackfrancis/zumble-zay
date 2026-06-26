# ADR 0014: The AxisRanker uses an OpenAI-compatible chat-completions client (Copilot default)

Status: Accepted — 2026-06-26

Realizes the LLM half of [0011](0011-llm-produced-axes-zz-blends.md) (the four
axes are LLM-produced; ZZ blends and ratifies) and rides the runtime contract of
[0009](0009-agent-runtime-contract-boundary.md) and the substrate seam of
[0012](0012-kubernetes-native-substrates-swappable.md).

## Context

ADR 0011 defined `worklist.AxisRanker` as the seam between ZZ's deterministic
blend and an LLM that proposes the four axes, and shipped a `StubRanker`
(proposes the baseline; ratifying it is a no-op). The pipeline ran end-to-end but
every `llm-rank` job's contribution literally read "baseline (stub ranker)" — no
real intelligence yet.

We want a real model behind the seam, with **configurable models** and minimal
new architecture. A sibling CNCF/Prow project (`willie-yao/prow-ai-dashboard`)
solved the same problem by speaking the **OpenAI-compatible chat-completions
API** to a configurable endpoint, treating the provider (GitHub Copilot, OpenAI,
Azure OpenAI, NIM, vLLM, Ollama) as a config value. A GitHub-App-style migration
is irrelevant here; this is about the *model* backend, not the GitHub credential
([ADR 0013](0013-stay-on-github-oauth-app.md)).

## Decision

Implement `AxisRanker` as a single **OpenAI-compatible chat-completions client**
(`internal/llm`), configured by three knobs — `AI_ENDPOINT`, `AI_MODEL`,
`AI_TOKEN` — so the provider is a config change, not a code change. The default
targets **GitHub Copilot** (`https://api.githubcopilot.com/chat/completions`,
model `claude-opus-4.8`); when the endpoint host is a Copilot host the client
sends the required `Copilot-Integration-Id` header.

- **One call, structured JSON.** Ranking is a single chat-completion per item
  asking for `{relevance, impact, engagement, urgency, confidence, rationale}`
  (each 0..1). No agentic tool loop — ZZ's ranker only needs scores, and the
  prompt is a few hundred tokens, so model context size is immaterial.
- **The model never has the last word.** The existing ratify/blend guardrails
  (confidence floor, deviation clamp) bound the proposal against the
  signal-based baseline (ADR 0011), so a misbehaving or prompt-injected model
  cannot hijack ordering. Values are also clamped to [0,1] on parse.
- **Runtime-only dependency.** `internal/llm` is imported only by the agent
  runtime (like `internal/github`); ZZ core depends solely on the `AxisRanker`
  interface, so no core package imports a model client (ADR 0006).
- **The token is a secret, injected by reference.** Endpoint and model ride the
  plain injection contract (`ZZ_AI_ENDPOINT`, `ZZ_AI_MODEL`); the token does
  not. In-process it rides `RunParams` directly; on Kubernetes the launcher
  injects it via a `Secret` reference (`ZZ_AI_TOKEN` ← `secretKeyRef`), so a
  long-lived model token never appears as a plain value in a Job/Pod spec.
- **Stub fallback.** With no token configured, the runtime uses `StubRanker`, so
  the cluster-free test suite and any no-key environment keep working unchanged.

## Consequences

- Swapping models or providers (Copilot ↔ Azure OpenAI ↔ a local gateway) is a
  config change; the ranker code is provider-neutral.
- The `llm-rank` stage now produces genuine LLM proposals; ZZ still owns the
  final number via the deterministic blend, preserving ADR 0011's safety
  property.
- Copilot is metered: a full rank pass is one call per shortlisted item, so cost
  and latency scale with the shortlist. Concurrency/caching are available knobs
  if needed.
- The refresh-on-vend GitHub path (ADR 0013) is unaffected; this is a separate
  credential (a model token), held as a ZZ secret rather than a vended provider
  credential.
