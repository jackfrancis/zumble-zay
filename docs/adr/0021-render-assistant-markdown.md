# ADR 0021: Render assistant Markdown to sanitized HTML

Status: Accepted — 2026-06-28

Extends [0018](0018-per-item-assistive-conversation.md)/[0019](0019-conversation-as-ephemeral-agent.md)
(the conversation UI) and [0020](0020-conversation-github-tool-use.md) (the
assistant reasons over untrusted, tool-fetched content). Relates to
[0016](0016-server-rendered-landing-page.md) (server-rendered UI, strict CSP).

## Context

The assistant replies in Markdown — headings, bold, lists, fenced code, tables,
links — which the thread UI rendered as raw plain text, so users saw literal
`**bold**` and ```` ``` ```` fences. Rendering it is the natural fix (and plain
text is valid Markdown, so it degrades gracefully).

The catch: the reply is **untrusted**. It is produced by an LLM from
attacker-influenceable input — PR bodies, comments, and now tool results
(ADR 0020) — so turning it into HTML with naive string substitution is an XSS
vector (`<script>`, `javascript:` links, `onerror=` handlers, `data:`/remote
images). The strict CSP (`default-src 'self'`) helps but is not sufficient by
itself (e.g. it does not stop a `javascript:` href).

## Decision

Render Markdown to **sanitized** HTML, server-side, at a single boundary.

- **Pipeline:** a small `internal/markdown` package renders with **goldmark**
  (GitHub-flavored: tables, strikethrough, autolinks, task lists) with raw-HTML
  passthrough **disabled**, then sanitizes the output with **bluemonday**
  (`UGCPolicy`: an allow-list that drops scripts, event handlers, styles, and
  unsafe URL schemes). Fully-qualified links get `target=_blank` +
  `rel="noopener noreferrer"`. It is the only place model text becomes HTML.
- **Shared by both render paths:** the server-rendered thread page calls it via a
  `markdown` template function (emitting `template.HTML`); the JSON poll endpoint
  (`GET /api/thread`) returns a sanitized `html` field per message, which
  `app.js` injects with `innerHTML` for the live-appended agent reply.
- **Scope:** only **agent** messages are rendered as Markdown. **User** messages
  stay plain text (escaped, `white-space: pre-wrap`) — we don't reformat the
  user's own input, and it matches the optimistic bubble the page shows before
  the server round-trip.
- **CSP unchanged:** the sanitizer strips inline scripts and `style` attributes,
  which the existing `default-src 'self'` already forbids; the rendered HTML uses
  only the vendored stylesheet's classes/element styles.

## Consequences

- Replies now read as intended (formatted lists, code blocks, linked PRs), which
  matters more now that the assistant cites tool findings (ADR 0020).
- **Two new dependencies** (goldmark, bluemonday + their transitive css libs) —
  the first beyond `oauth2` and `client-go`. Justified: safe Markdown rendering
  is genuinely hard to hand-roll, and these are the de-facto-standard Go
  libraries. They are imported only by server-side packages (`webui`, `api`), so
  they never enter the agent **runtime** binary.
- XSS is contained at one audited boundary with tests covering script tags,
  `javascript:` links, event handlers, and raw-HTML escaping. Raw HTML in a reply
  is escaped (shown as text), never executed.
- A reply embedding a remote image renders a broken image (CSP blocks the load) —
  accepted; images are rare in triage answers and there is no exfiltration.
- The optimistic user bubble stays plain text (the browser has no Markdown
  renderer, by design — no client-side JS libs); since user input is plain text
  this is unnoticeable, and a reload renders the full thread server-side.
