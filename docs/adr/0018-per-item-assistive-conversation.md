# ADR 0018: Per-item assistive conversation (read-only)

Status: Accepted — 2026-06-26

> The **synchronous, in-server turn** below is superseded by
> [0019](0019-conversation-as-ephemeral-agent.md), which answers each turn with a
> spawned `github-converse` agent that reads live GitHub context. The data model,
> read-only stance, seam, and UI shape carry forward unchanged.

Builds on [0006](0006-credential-broker-not-data-broker.md) (read-only,
credential-broker), [0011](0011-llm-produced-axes-zz-blends.md)/[0014](0014-axis-ranker-openai-compatible-client.md)
(the chat model), [0013](0013-stay-on-github-oauth-app.md) (read-only GitHub),
and [0016](0016-server-rendered-landing-page.md)/[0017](0017-hide-user-metadata-agents-unhide.md)
(the UI and item-scoped writes).

## Context

From a work item the user wants to start an agentic conversation — e.g. "draft a
review request," "summarize this discussion," "who should review this?" — and
**retain that conversation on the item record**. The relationship with GitHub is
**read-only**: the assistant may advise and draft, but ZZ never acts on the
provider.

## Decision

Add a **read-only, assistive** per-item conversation.

- **Data:** `WorkItem.Thread []Message{Role, Content, At}`, persisted on the item
  and **preserved across re-ingest** (agents never author it, so the stored
  thread is authoritative — same merge point as `HiddenAt`, ADR 0017).
- **Seam:** a `worklist.Conversationalist` interface in ZZ core, implemented by
  `llm.Converser` and injected at the composition root, so `internal/server` and
  `internal/api` import only the interface — no core package imports a model
  client (ADR 0006, the `AxisRanker` pattern of ADR 0011).
- **Turn:** `POST /api/thread?id=<item id>` (the id rides the query, URL-encoded,
  because ids contain `/#`). The handler appends the user message, asks the
  conversationalist for a reply **synchronously**, appends it, and persists both.
  The converser reasons only over the item's **existing ZZ data + the thread**;
  it fetches nothing and calls no GitHub write.
- **UI:** a "Discuss" link opens a thread page that server-renders prior
  messages; `app.js` posts turns with `fetch` and appends them in place. It is
  same-origin, so `default-src 'self'` already permits it — no CSP change.
- **Unconfigured:** with no AI token the conversationalist is nil and the
  endpoint reports the assistant unavailable; the rest of the app is unaffected.

## Consequences

- The first conversational capability, and it stays inside the project's core
  invariant (sources are READ). Because it is read-only and advisory, the
  prompt-injection risk is low-severity: malicious item content could only skew a
  **draft the user reviews**, never trigger an action — the danger ADR 0015
  deferred (read untrusted content *and* act) does not apply here.
- It raises the priority of real persistence (roadmap #5): threads are unbounded
  text in an in-memory, `replicas: 1` store.
- The turn is synchronous, so the request is held for the model's duration; if
  replies get slow, streaming or an async (spawned-runtime) turn is the next
  step — and the same `Conversationalist` interface allows moving the turn into a
  spawned `converse` runtime for the Kubernetes substrate later.
- Richer GitHub context (PR body, comments, diff) is deferred: it needs a vended
  read credential + the provider client (a runtime concern), and it is the
  attacker-controllable surface — so it pairs with the discovery/injection design
  (ADR 0015), not this first read-only slice.
