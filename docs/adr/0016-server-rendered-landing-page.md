# ADR 0016: A lean server-rendered landing page styled with vendored Primer

Status: Accepted — 2026-06-26

Realizes the landing page anticipated in [ARCHITECTURE](../ARCHITECTURE.md) and
[0004](0004-render-reads-persisted-zz.md) (render reads persisted ZZ data, not
live GitHub).

## Context

ZZ served only JSON (`GET /api/worklist`). The product needs the landing page
that renders the user's work ordered by ZZ metadata. Two shaping forces:
the project ethos is a **lean, secure, single-binary Go service** (`replicas: 1`,
in-memory), and the UI should **resemble GitHub** so it feels native to the
domain.

GitHub maintains an official design system, **Primer** (`@primer/css` +
`@primer/primitives` for the design tokens, Octicons for icons). Leaning on it
gets an authentic look; the question was how to do so without a JS build
pipeline, a CDN dependency, or weakening the strict API CSP.

## Decision

Ship a **server-rendered HTML landing page** from the existing Go server:

- **One embedded `html/template`**, three views — sign-in (anonymous),
  processing (a backfill is running), and the ranked worklist — selected by the
  handler. Served at `GET /{$}`; static assets at `GET /static/`. Everything is
  compiled into the binary via `go:embed`, so there is no separate build or
  deploy artifact.
- **Vendored Primer, served from the binary.** `make vendor-primer` downloads
  pinned `@primer/css` + the needed `@primer/primitives` files into
  `internal/webui/static/primer`; they are committed and embedded, so the page
  loads them from `/static` (same-origin) — **no CDN and no runtime egress**.
  Updates are a version bump + re-run, as the maintainer wants.
- **Shared read model.** The page and the JSON API both call `worklist.Resolve`
  (load → rescore agent items against now, preserving human overrides → sort, or
  trigger backfill), so the two cannot drift.
- **CSP stays tight.** The page uses only same-origin external stylesheets (no
  inline styles — the rank bar uses bucketed width classes) and **no JavaScript**
  (the processing view auto-refreshes via `<meta http-equiv="refresh">`). So the
  global CSP relaxes only from `default-src 'none'` to
  `default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`
  — no `unsafe-inline`, no third-party origins.
- **App styling rides Primer's tokens.** A small `app.css` lays out the page
  using Primer's stable CSS-variable tokens (`--bgColor-default`,
  `--fgColor-muted`, …) plus a few Primer component classes (`Box`, `Label`,
  `btn`), so it tracks vendored Primer rather than hard-coding colors.

## Consequences

- A genuine GitHub-resembling UI with **no JS toolchain** and a single binary;
  the embedded Primer adds ~1.2 MB to the binary (gzips to ~120 KB on the wire,
  cached same-origin).
- The headless path is unchanged: no token still yields baseline ranking, and
  the page simply renders whatever `Resolve` returns.
- Deferred niceties: sort controls (the API already supports `sort`/`order`),
  logout from the header, Octicons, and dark mode (light theme only for now).
  A heavier client-side SPA remains possible later behind the same JSON API
  without touching the backend.
- **The worklist renders only once the pipeline settles.** While a pass is in
  flight (`orchestrator.Active(user)`), the page shows the *processing* view and
  polls via `<meta http-equiv="refresh">`; the ranked list is rendered only when
  the pipeline (last stage: llm-rank) has settled, and is then fully static (no
  JS, no polling). So the user gets one clean transition to the final ranking
  instead of watching a half-ranked list churn — the tradeoff is waiting for the
  first content rather than seeing an early baseline list. A background re-rank
  that starts *after* the page is static — e.g. a future **cron re-ranking** —
  would not be picked up until a manual refresh. Closing that needs either an
  always-on slow meta-refresh when idle (lean, no JS, costs a periodic full
  reload) or a small JS poller / SSE against a status endpoint (more efficient,
  reintroduces `script-src`). The server data is already live via `Resolve`;
  only the browser's re-fetch trigger is missing.
- `make vendor-primer` is the update path; bumping `PRIMER_*_VERSION` and
  re-running scoops in new Primer releases.
