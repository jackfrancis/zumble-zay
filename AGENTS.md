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

- Go 1.25. Third-party deps: `golang.org/x/oauth2` (auth); `k8s.io/client-go`
  (the `k8s-job`/`k8s-pod` launcher only — compiled into the **orchestrator**,
  never the web server or the agent runtime; see ADR 0012, 0023); and
  `yuin/goldmark` + `microcosm-cc/bluemonday` (render
  + sanitize the assistant's Markdown for the UI — server-only, never the runtime;
  see ADR 0021).
- Module: `github.com/jackfrancis/zumble-zay`.

```
cmd/server/          web tier entrypoint (HTTP, UI, /agent/* sink, control client)
cmd/orchestrator/    control-plane entrypoint: launcher + minter + control API (ADR 0023)
internal/config/     env configuration
internal/session/    server-side sessions over HMAC-signed cookies
internal/auth/        OAuth provider wiring + login/callback
internal/authn/      unified auth: RequireAuth/RequireScope; cookie OR bearer
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/mint/       Ed25519 job tokens: Minter (orchestrator) + Verifier (web) (ADR 0023)
internal/controlplane/ web↔orchestrator boundary: Client, Local, HTTP, Handler (ADR 0023); token exchange (ADR 0024)
internal/launcher/    substrate registry: Register/Build, driver pattern (ADR 0024)
internal/orchestrator/ co-located/standalone control plane: jobs, mint, launcher seam (sync + async)
internal/worklist/   WorkItem + ZZ Metadata, Store + Ingestor seams, sort
internal/api/        JSON HTTP handlers (worklist)
internal/server/     web-tier router + middleware (no launcher, no client-go)
deploy/k8s/          kustomize base + dev overlay (web + orchestrator Deployments)
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
- Job tokens are asymmetric (Ed25519, ADR 0023): the **orchestrator** is the
  sole issuer (private key); the web tier holds only the public key and
  **verifies** — it must never gain minting ability. The web→orchestrator control
  API is cluster-internal, bearer-authenticated (`CONTROL_PLANE_TOKEN`),
  fail-closed, and never exposed through the Ingress. Pod/Job-creation RBAC binds
  to the orchestrator ServiceAccount only, never the web tier's. The pull
  token-exchange endpoint (`POST /control/token`, ADR 0024) issues only
  policy-scoped, single-user, short-TTL job tokens, and authenticates the caller
  behind the `CallerAuthenticator` seam (shared bearer today; per-service
  platform OIDC is the hardening).
- Token vault encrypted at rest (KMS) once persisted. Agents connect to
  providers **directly** with a short-lived credential **vended by ZZ on demand**
  (ADR 0006); ZZ never proxies provider data. ZZ core packages must not import a
  provider client — only agent runtimes do. (GitHub uses the OAuth App; its
  vended token is long-lived — an accepted, read-only/public tradeoff, since a
  GitHub App is installation-scoped and cannot serve the cross-org radar. See
  ADR 0013.)
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
- **Adding an agent substrate is additive; the in-cluster baseline must not
  regress.** The reference launchers (`inprocess`, `k8s-job`, `k8s-pod`,
  `k8s-pod-detached`) and the runtime contract (`agent.ZZClient` + the `ZZ_*`
  injection) are the validated in-cluster baseline. A new substrate is a new
  `orchestrator.Launcher` (optionally `AsyncLauncher`) + a `launcher.Register`
  from its package init + a blank import in `cmd/orchestrator` — it must not modify
  the existing launchers. An **optional** substrate that pulls a heavy or uncommon
  dependency lives behind a build tag (e.g. `agent-sandbox`, `//go:build
  agent_sandbox`, ADR 0026): it self-registers, is blank-imported in
  `cmd/orchestrator` under the same tag, and is absent from the default build. Any
  change to a shared seam (the `Launcher`/
  `AsyncLauncher` interfaces, the `ZZClient`/`ZZ_*` contract, the shared
  `internal/runtimespec` helpers — the runtime container/env/labels every
  substrate emits — `JobSpec`, or the orchestrator's
  dispatch/completion path) must be backward-compatible and keep the reference
  launchers' tests green — identical pipeline output across `inprocess`/`k8s-job`/
  `k8s-pod` is the regression check (ADR 0012, 0024).
- **Concurrent substrate work stays merge-clean.** Multiple new substrates in
  parallel (e.g. ray/kuberay and opensandbox) are conflict-free as long as each
  touches only its *own* files: a new `internal/<substrate>` package and its
  *own* `//go:build`-tagged blank-import file in `cmd/orchestrator` (never a
  shared one). Keep substrate-specific knobs (endpoints, API keys, pinned
  versions) in the launcher's own `build()`/package rather than adding them to
  `internal/config` — a remote control-plane substrate needs no shared config at
  all. The Makefile's `ORCHESTRATOR_GO_TAGS` is append-friendly (`+=` one line
  per tagged substrate); per-substrate cluster prerequisites (RBAC rules,
  controller installs) are additive, never edits to the base defaults (`LAUNCHER`
  stays `k8s-job`). Reserve the next free ADR number and add its README row in
  your first PR so two efforts don't claim the same one — reserved so far:
  **0027** opensandbox, **0028** ray/kuberay.

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

1. **Harden the GitHub credential in place (ADR 0013).** A GitHub App is ruled
   out — its user-to-server tokens are installation-scoped and cannot read the
   cross-org repos the radar depends on. Stay on the read-only/public OAuth App
   and bound the exposure instead: revoke the stored credential on logout, and
   encrypt the vault at rest (lands with persistence, since the vault is
   in-memory today).
2. Widen GitHub coverage to private repos (`repo` scope) and more signal queries.
3. **DONE — orchestrator extracted into its own runtime + identity (ADR 0023).**
   `cmd/orchestrator` is a separate Deployment with the Pod/Job-creation RBAC and
   the Kubernetes client; the web tier sheds both (0 client-go). The web tier
   reaches it through `controlplane.Client` (co-located `Local` or remote `HTTP`
   control API). Job tokens are now asymmetric (Ed25519): the orchestrator is the
   sole issuer, the web tier verifies only. Remaining: move the reconciler to the
   orchestrator once the store is shared (#5); independent mint keys for true
   issuer/verifier key separation in production.
4. ZZ metadata write path beyond ingestion: `PATCH /api/datapoints/{id}/metadata`,
   scope-gated, with provenance and optimistic concurrency.
5. Cloud persistence behind `worklist.Store`; shared session store; then scale
   `replicas > 1`.
6. Additional agent substrates behind the `Launcher` seam (ADR 0012 step 7).
   **DONE — `agent-sandbox` (isolation), an optional build-tagged substrate
   (ADR 0026):** `internal/agentsandbox` creates a `Sandbox` CR
   (`agents.x-k8s.io/v1beta1`) via the client-go dynamic client (no new module),
   detached completion with native self-reap, gated behind `//go:build
   agent_sandbox` and selected with `LAUNCHER=agent-sandbox`; the shared runtime
   shape moved to `internal/runtimespec`. Remaining: `KueueLauncher`
   (admission/quota) and `KagentLauncher`. The plumbing is in place (ADR 0024): a
   launcher
   **registry** (`internal/launcher`, driver pattern — implement `Launcher`,
   `Register` from a package init, blank-import in `cmd/orchestrator`), **async**
   dispatch (`AsyncLauncher` Dispatch/Await, so a slow job never pins a worker
   and completion is decoupled from `Launch` returning), and an RFC 8693-flavored
   **token-exchange** endpoint (`POST /control/token`) for service runtimes
   (kagent) that pull a per-job token. The remaining substrate launchers are
   additive TODOs; the four in-cluster launchers plus `agent-sandbox` are the
   references. **DONE — `opensandbox` (ADR 0027):** `internal/opensandbox` is the
   first *remote-control-plane* substrate — a hand-rolled `net/http` client of an
   OpenSandbox lifecycle server (no new module, no build tag, no ZZ RBAC; the
   endpoint + API key are read in `build()` from `OPENSANDBOX_ENDPOINT`/
   `OPENSANDBOX_API_KEY`, so `internal/config` is untouched), detached +
   callback completion (ADR 0025) with the job deadline mapped onto the sandbox
   self-reap timeout; it reuses the shared `runtimespec.Env` injection map.
   Runtimes report terminal completion (`POST /agent/complete`) which the
   orchestrator races against the launcher watch (ADR 0025), so the k8s launcher
   is callback-driven with no code change. Remaining: per-service caller identity
   (platform OIDC) for token exchange. An await-the-deadline launcher
   (`k8s-pod-detached`) is the in-cluster reference for a fully detached substrate
   — dispatch + callback-only completion, create-only RBAC, deadline backstop.
7. **Prompt & context tuning is now the primary correctness lever (ADR 0015).**
   The LLM is authoritative for the four axes, so ranking quality is steered in
   `internal/llm/prompt.go` — sharper axis definitions, the user's priorities and
   role, repo strategic tiers, calibration examples — not by numeric guardrails.
   Feeding richer per-item context (issue/PR bodies, recent comments) raises the
   ceiling but is coupled to the deferred discovery/injection design (ADR 0015),
   since that content is the attacker-controllable surface.

## Open questions

- Worklist ordering: a single agent-computed `rank`, or client-selectable
  multi-key sort? (Endpoint already supports `sort`/`order`.)
- Placement of items lacking ZZ metadata (hidden vs default rank).
- Orchestrator root-identity details on the chosen K8s platform.
