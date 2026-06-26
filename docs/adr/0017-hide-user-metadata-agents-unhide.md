# ADR 0017: Hide is user-set metadata; agents auto-unhide on change

Status: Accepted — 2026-06-26

Builds on [0008](0008-worklist-ranking-signals-vs-scores.md) (signals vs.
ZZ metadata), [0011](0011-llm-produced-axes-zz-blends.md)/[0015](0015-llm-axes-authoritative.md)
(scoring), and [0016](0016-server-rendered-landing-page.md) (the UI). It is the
first **metadata write from the UI**.

## Context

A user wants to dismiss an item from their radar, but a dismissed item should
**resurface when the underlying GitHub item changes** (new commits, a new
comment, a title/label/state change) — otherwise "hide" silently buries work
that later becomes relevant again.

GitHub supports detecting that change cheaply: an issue/PR's `updated_at` is
bumped by title/body edits, labels, assignees, state changes, and **new
comments** — all already fetched by the ingester and stored as
`GitHub.UpdatedAt`. (PR commit pushes generally bump it too; the enrich
timeline is a definitive backstop. Reactions notably do **not** bump it, which
is fine — a reaction should not resurface an item.)

## Decision

Model "hidden" as a timestamp, `Metadata.HiddenAt`: zero = visible, a timestamp
= hidden (and when). The user sets it; an agent clears it.

- **Hide (UI).** Each item has a Hide button (a form) posting to
  `POST /items/hide`. The handler checks the session, sets `HiddenAt = now` for
  that item, and redirects back (Post/Redirect/Get). Confirmation is a small
  same-origin `app.js` that intercepts forms with `data-confirm` — no inline
  script, so the CSP is unchanged (`default-src 'self'` already permits
  same-origin scripts). It is progressive enhancement: with JS off, the form
  still submits (without the prompt).
- **Filter (read).** `worklist.Resolve` drops items with a set `HiddenAt` from
  the user-facing list, but they **stay in the store** so agents can still see
  and update them. The agent read path (`GET /agent/worklist`) does not filter.
- **Auto-unhide (ingest).** On every agent write, the ingest handler carries a
  prior `HiddenAt` forward via `worklist.HiddenAfter(prev, updatedAt)`, which
  **clears it when `GitHub.UpdatedAt` is after the hidden time** — so a changed
  item resurfaces on the next ingest. Hidden state is thus preserved across
  re-ingest and rescore rather than being wiped.

## Consequences

- The radar stays uncluttered without losing work: dismissed items return
  automatically the moment they get activity.
- This is the first user-originated metadata write; `SameSite=Lax` session
  cookies give baseline CSRF protection for the state-changing POST (a
  cross-site POST carries no cookie, so it no-ops).
- **Known limitation — a read-modify-write race.** Hide (list → set → upsert)
  and an in-flight ingest (which also upserts) can interleave on the in-memory
  store, so a hide could rarely be clobbered by a concurrent ingest that didn't
  observe it. Acceptable at `replicas: 1` for a personal tool; the real fix is a
  store-level single-field update or optimistic concurrency (roadmap #4).
- Only **auto** unhide exists; a manual "unhide" control can be added later
  (the list filter and field already support it).
- `HiddenAt` is independent of the `Origin` rescore guard: a hidden item is
  still rescored normally; only its visibility changes.
