# AGENTS.md — Zumble-Zay project context

Canonical, tool-neutral context for humans and AI agents working in this repo.
Copilot loads this via `.github/copilot-instructions.md`; other tools
(Claude, Cursor, Aider, etc.) read `AGENTS.md` directly. Keep it current.

For deeper material see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), the
decision records in [docs/adr/](docs/adr/), and the LLM stress-test harness in
[docs/DESIGN_REVIEW.md](docs/DESIGN_REVIEW.md).

## What this is

Zumble-Zay (ZZ) is a lean, secure Go backend. It is the first component of a
larger system that retrieves a user's GitHub data (issues, PRs, comments,
reviews), persists it, and decorates each item with **ZZ-specific metadata**
(priority, relevance, impact) produced by AI agents. A landing page renders the
user's work ordered by that metadata.

Status: **foundation only**. Auth, sessions, the principal/authorization model,
an extensible worklist endpoint, and a Kubernetes dev workflow exist. GitHub
data retrieval, persistence, the agent/orchestrator runtime, and metadata
writes are **designed but not built**.

## Tech & layout

- Go 1.25. Third-party deps: `golang.org/x/oauth2` (auth) and `k8s.io/client-go`
  (the `k8s-job` launcher only — compiled into the **server**, never the agent
  runtime; see ADR 0012).
- Module: `github.com/jackfrancis/zumble-zay`.

```
cmd/server/          entrypoint (http.Server, graceful shutdown)
internal/config/     env configuration
internal/session/    server-side sessions over HMAC-signed cookies
internal/auth/        OAuth provider wiring + login/callback
internal/authn/      unified auth: RequireAuth/RequireScope; cookie OR bearer
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/worklist/   WorkItem + ZZ Metadata, Store + Ingestor seams, sort
internal/api/        JSON HTTP handlers (worklist)
internal/server/     router + middleware
deploy/k8s/          kustomize base + dev overlay
```

## HTTP endpoints

| Method | Path | Auth | Notes |
| --- | --- | --- | --- |
| GET | `/healthz` | no | health |
| GET | `/auth/providers` | no | enabled providers |
| GET | `/auth/{provider}/login` | no | begin OAuth (PKCE+state) |
| GET | `/auth/{provider}/callback` | no | completes login |
| POST | `/auth/logout` | no | destroy session |
| GET | `/api/me` | yes | current principal |
| GET | `/api/worklist` | yes | ordered work items; `?sort=`, `?order=` |

## Core design decisions (see docs/adr for rationale)

- **ZZ is the authorization server.** It issues short-lived, scoped tokens to
  agent runtimes (OAuth 2.0 Token Exchange / RFC 8693 shape). It also owns user
  consent and an encrypted **token vault** of delegated provider tokens. ZZ is a
  **credential broker, not a data broker**: it vends those provider tokens to
  runtimes on demand but never proxies provider data (ADR 0006).
- **Agents are ephemeral workload identities.** An orchestrator (control plane)
  spawns single-purpose agent runtimes; each is an integral unit that receives
  a ZZ-minted, job-scoped token. Authorization is minted at spawn:
  `orchestrator authority ∩ user consent ∩ runtime-type policy`.
- **Kubernetes is the substrate.** Orchestrator starts with a managed
  client-secret, evolves to projected ServiceAccount OIDC. Spawned runtimes
  carry ZZ-minted tokens, not platform identities.
- **The landing page reads persisted ZZ data, not live GitHub.** GitHub fetch
  is ingestion (an agent job), decoupled from rendering. An empty worklist GET
  triggers `Ingestor.EnsureBackfill` and returns `status: processing`.
- **Principals unify users and workloads.** Downstream handlers see a
  `Principal`; they do not care whether a cookie or bearer token authenticated.

## Security invariants (do not regress)

- OAuth uses PKCE + `state`. Session ID rotates on login (anti-fixation).
- Session cookies: HMAC-signed, HttpOnly, SameSite=Lax; `Secure` in prod.
- A presented-but-invalid bearer token returns 401 and **never** falls through
  to the cookie session.
- Bearer/runtime tokens: `Authorization: Bearer` header only (never query
  string), TLS only, short TTL, revocable; store only hashes if opaque.
- Token vault encrypted at rest (KMS) once persisted. Agents connect to
  providers **directly** with a short-lived credential **vended by ZZ on demand**
  (ADR 0006); ZZ never proxies provider data. ZZ core packages must not import a
  provider client — only agent runtimes do. (First slice uses the GitHub OAuth
  App, so the vended token is long-lived — a tracked limitation.)
- Least privilege: source scopes are READ (GitHub `repo`, Graph `Mail.Read`,
  `Chat.Read`); only ZZ metadata is WRITE.
- Metadata writes are attributed (runtime → job → user → signals) and marked
  agent-derived vs user-set; humans can override.

## Conventions & gotchas

- **Container image name is `localhost/zumble-zay:dev` everywhere.** Podman
  auto-prefixes `localhost/`; using the bare name causes a `docker.io/library`
  normalization mismatch and ImagePullBackOff in kind.
- **`replicas: 1` is intentional.** Sessions and the worklist are in-memory, so
  pods cannot share state. Do not raise replicas until a shared session store
  and the cloud worklist store exist.
- Each `.go` file must have exactly one `package` clause. An external
  format-on-save tool has repeatedly injected a duplicate `package` line above
  the doc comment; if a build fails with "non-declaration statement outside
  function body", remove the stray line.
- Interfaces are the extension seams: `authn.TokenValidator` (workload tokens),
  `worklist.Store` (cloud persistence), `worklist.Ingestor` (agentic backfill).
  Default impls are deliberate placeholders.
- Adding a worklist sort = add a `SortKey` constant + one comparator in
  `internal/worklist/sort.go`.

## Build / dev / test

```sh
make test            # unit tests
make vet             # static analysis
make build           # compile to bin/server
make dev-up          # kind: build from source -> load -> apply -> rollout
make dev-forward     # port-forward to localhost:8080
make dev-logs        # tail logs
make dev-down        # delete the kind cluster
```

Container engine auto-detected (podman preferred, docker fallback); override
with `CONTAINER_ENGINE=docker`. After any `.go` edit, run `make test` and
`go vet ./...`.

## Roadmap (next increments)

The agentic ingestion slice is in progress: a co-located orchestrator spawns an
ephemeral runtime that connects to GitHub **directly** with a ZZ-vended
credential and writes results back to ZZ (ADR 0006, 0007).

1. **Harden the GitHub credential (priority).** Migrate from the OAuth App
   (long-lived user tokens) to a **GitHub App** with expiring user-to-server
   tokens (refresh tokens in the vault) so each job gets a short-lived
   credential. Do this before agents run out-of-process. (ADR 0006)
2. Widen GitHub coverage to private repos (`repo` scope) and more signal queries.
3. Extract the orchestrator into its own runtime + identity once it spawns real
   workloads or ZZ scales past `replicas: 1`. (ADR 0007)
4. ZZ metadata write path beyond ingestion: `PATCH /api/datapoints/{id}/metadata`,
   scope-gated, with provenance and optimistic concurrency.
5. Cloud persistence behind `worklist.Store`; shared session store; then scale
   `replicas > 1`.

## Open questions

- Worklist ordering: a single agent-computed `rank`, or client-selectable
  multi-key sort? (Endpoint already supports `sort`/`order`.)
- Placement of items lacking ZZ metadata (hidden vs default rank).
- Orchestrator root-identity details on the chosen K8s platform.
