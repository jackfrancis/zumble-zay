# ADR 0031: Control-plane per-service caller identity via TokenReview

Status: Accepted — 2026-07-09. Implemented in `internal/controlauth` (the
TokenReview `Authenticator`), `internal/controlplane` (a single
`CallerAuthenticator` across **all** control routes, `NewChainCallerAuthenticator`,
and the `HTTP` client's rotating token source), `internal/config`
(`CONTROL_PLANE_AUDIENCE`, `CONTROL_PLANE_CALLERS`, `CONTROL_PLANE_TOKEN_PATH`),
`cmd/orchestrator` + `cmd/server` (wiring), and `deploy/k8s` (base: the
`system:auth-delegator` binding; dev overlay: the web tier's projected token +
the audience/callers config). Green across the suite; TokenReview needs no CNI, so
the path is exercisable on kindnet — pending live validation.

Builds on [0023](0023-orchestrator-own-runtime-and-identity.md) (the orchestrator
got its own identity and a cluster-internal, bearer-authenticated control API; the
internet-facing web tier holds no Kubernetes client),
[0024](0024-agent-runtime-portability.md) (which introduced the
`CallerAuthenticator` seam and named per-service platform OIDC as the hardening of
the shared bearer), and [0030](0030-job-token-pull-path.md) (whose remaining
exposure was explicitly "transport and caller identity" — this addresses the
caller-identity half).

## Context

The control API is the web→orchestrator boundary: the web tier calls it to trigger
privileged work (ingest a worklist, answer a conversation turn, re-rank, exchange a
token, redeem a ticket) and the orchestrator, the sole holder of Pod/Job-creation
RBAC and the mint key, acts on it. Since ADR 0023 it is cluster-internal (ClusterIP,
never the Ingress) and authenticated by a **single shared bearer**,
`CONTROL_PLANE_TOKEN`.

Four things shaped the change.

1. **The shared bearer is coarse.** One long-lived secret is copied into both tiers'
   environments. It carries no caller identity (the orchestrator authenticates
   "whoever holds the secret," not "the web tier"), cannot be revoked per caller, and
   is equally valid on every route. A leak from either tier's Secret or env
   compromises the whole control plane, including token exchange and ticket
   redemption.
2. **ADR 0024 already named the fix and left the seam.** Authentication was routed
   through a `CallerAuthenticator` for token exchange only; the trigger routes and
   `/control/redeem` still checked the shared bearer inline. ADR 0024 called out
   "per-service platform OIDC" as the intended hardening, and ADR 0030 deferred "a
   per-service OIDC identity for the web→orchestrator redeem."
3. **The web tier must stay client-free (ADR 0023).** The validation logic needs a
   Kubernetes client (TokenReview), which must **not** enter the internet-facing web
   tier's import graph. So the verifier lives only in the orchestrator.
4. **The dev CNI does not enforce NetworkPolicy.** kind's default kindnet ignores
   NetworkPolicy, so the *transport-isolation* half of control-plane hardening
   (NetworkPolicy / mTLS) cannot be validated locally. Caller identity via TokenReview
   needs **no** CNI support, so it is the increment that is both a real hardening and
   locally testable on kindnet.

Why TokenReview rather than validating a JWT against JWKS: it is Kubernetes-native.
The apiserver is the issuer and the verifier of its own projected ServiceAccount
tokens, so ZZ imports no JWT library, distributes no keys, and adds no module — the
TokenReview types are a sub-package of the `k8s.io/api` dependency already present,
and the typed clientset is already used by the orchestrator.

## Decision

Give each tier its own Kubernetes workload identity and have the orchestrator
authenticate the web tier by validating its projected ServiceAccount token, behind
the existing seam, chained over the shared bearer for a fail-safe rollout.

- **One `CallerAuthenticator` guards every control route.** The handler now
  authenticates all routes (triggers, token exchange, redeem) through a single
  `h.caller` and stashes the resolved `Caller` in the request context; `exchange`
  reads it from context instead of re-authenticating. One override (`WithCaller`)
  swaps the mechanism for the whole surface, so identity can never be enforced on
  some routes and skipped on others.
- **`controlauth.Authenticator` validates a projected token via TokenReview.** The
  web tier mounts a projected ServiceAccount token scoped to
  `audience=zumble-zay-orchestrator` and presents it as the control-API bearer. The
  orchestrator calls TokenReview for that audience and **fails closed** unless the
  token is authenticated, its returned audiences include ours (blocking replay of a
  token minted for another service), and the caller's ServiceAccount username is on
  the `CONTROL_PLANE_CALLERS` allowlist. It returns `Caller{Subject: <SA username>,
  Trusted: true}`.
- **The verifier stays out of the web tier.** `controlauth` is its own package,
  **not** part of `internal/controlplane` (which the web tier imports), so the client-go
  dependency it needs never reaches the internet-facing process. Only
  `cmd/orchestrator` imports it (ADR 0023 preserved).
- **Chain over the shared bearer — additive and reversible.** The orchestrator wires
  `NewChainCallerAuthenticator(tokenReview, sharedBearer)`: per-service identity
  first, the shared bearer as fallback. Co-located runs, tests, and a not-yet-migrated
  caller keep working. With `CONTROL_PLANE_AUDIENCE` unset the handler is exactly the
  old shared-bearer path, so the base default does not regress.
- **The web tier re-reads the token per request.** A projected token is rotated in
  place by the kubelet, so `NewHTTPWithTokenSource` reads the mounted file on each
  call (`CONTROL_PLANE_TOKEN_PATH`) rather than capturing it once — a file source, not
  an env var, precisely because the value changes.
- **Base carries the RBAC as an unused capability; the overlay turns it on.** Base
  adds a `system:auth-delegator` ClusterRoleBinding for the orchestrator
  ServiceAccount — read-only auth delegation (the ability to call TokenReview), **not**
  spawn power, and inert until `CONTROL_PLANE_AUDIENCE` is set — mirroring the
  "harmless when unused" sandbox rule already in `rbac.yaml`. The dev overlay adds the
  projected-token volume + `CONTROL_PLANE_TOKEN_PATH` on the web tier and
  `CONTROL_PLANE_AUDIENCE` / `CONTROL_PLANE_CALLERS` for the orchestrator, so
  `make dev-up` runs the hardened path.

## Consequences

- **Each tier presents its own identity.** The orchestrator authenticates the web
  tier as a specific ServiceAccount, not as a secret-bearer. A short-lived, rotating,
  audience-bound token replaces a long-lived shared secret on the primary path, and
  the caller can be revoked (drop it from the allowlist) without rotating a secret
  shared by both tiers.
- **Real hardening that is testable without a CNI.** TokenReview is core Kubernetes,
  so `make dev-up` on kindnet exercises the full path end to end; the transport half
  (NetworkPolicy / mTLS) stays a separate follow-up, since kindnet will not enforce
  NetworkPolicy — that needs a policy-enforcing CNI or a production cluster.
- **Additive and reversible.** The base default is unchanged (shared bearer); the
  chain fallback keeps co-located, test, and migration paths green. Because the seam
  is uniform, a third mechanism (mTLS/SPIFFE, a cloud workload identity) is just
  another `CallerAuthenticator` — no route-by-route rework.
- **Token exchange is now identity-aware too.** `/control/token` (ADR 0024) and
  `/control/redeem` (ADR 0030) inherit the same per-service check, so the pull-path's
  deferred caller-identity gap is closed on the web→orchestrator hop.
- **No new module, secret, or minting power.** TokenReview reuses the `k8s.io/api`
  and client-go already in the orchestrator; the web tier gains a mounted token but no
  client and no mint key (ADR 0023 intact).
- **Remaining.** Retire the shared-bearer fallback once every caller presents
  identity (the chain drops to TokenReview-only and `CONTROL_PLANE_TOKEN` can be
  removed); extend per-service identity to the runtime→web `/agent/*` plane (still
  coarse); and add transport isolation (mTLS / NetworkPolicy) on a policy-enforcing
  substrate. Like the ticket store (ADR 0030), nothing here changes the
  `replicas: 1` posture (ADR 0005).
