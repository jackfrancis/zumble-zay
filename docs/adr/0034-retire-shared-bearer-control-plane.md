# ADR 0034: Retire the shared-bearer control-plane fallback (TokenReview-only)

Status: Accepted — 2026-07-09. Implemented in `internal/controlplane` (a single
injected `CallerAuthenticator`, a fail-closed deny-all default, and an HTTP client
that presents a projected-token source), `cmd/orchestrator` (requires
`CONTROL_PLANE_AUDIENCE`, authenticates via TokenReview only), `cmd/server`
(requires `CONTROL_PLANE_TOKEN_PATH`), `internal/config` (the `ControlPlaneToken`
field is gone), and the base manifests (the per-service identity is the default,
not a dev-overlay opt-in). Green across the suite, `go vet`, and `kubectl
kustomize` for base + dev. Not yet run on live kind — the projected-token /
TokenReview path is validated on a cluster by the operator.

Completes [0031](0031-control-plane-caller-identity.md) (which introduced the
per-service TokenReview identity but chained it *over* the shared bearer as a
migration fallback). Builds on [0023](0023-orchestrator-own-runtime-and-identity.md)
(the split control plane and its original shared bearer) and
[0033](0033-control-plane-transport-isolation.md) (the NetworkPolicy that fronts
the control port).

## Context

ADR 0031 gave the web→orchestrator control API a per-service caller identity — a
projected ServiceAccount token validated by TokenReview — but kept the original
shared bearer (`CONTROL_PLANE_TOKEN`) as a *chained fallback*: TokenReview first,
the shared secret second, "for co-located and test runs and migration."

That fallback is a standing shared secret that, on its own, grants the full
control API: privileged agent spawns **and** job-token minting. It is exactly the
kind of long-lived crown-jewel credential the rest of the design works to avoid
(short-TTL job tokens, audience binding, per-service identity). It also *masks*
misconfiguration: a broken OIDC path silently succeeds via the bearer, so an
operator cannot tell whether per-service identity actually works.

Two things changed the balance since 0031. The OIDC path is live-validated (0031
ran clean on the agent-sandbox split control plane), and the control port is now
network-fronted by a default-deny NetworkPolicy (0033). The fallback's residual
value (a break-glass secret, a non-Kubernetes escape hatch) is low; its residual
risk (a shared secret that fully authorizes the control plane) is not.

## Decision

Retire the shared bearer entirely. The control plane authenticates every caller
solely by its Kubernetes workload identity; there is no shared-secret path.

- **One injected authenticator.** `controlplane.NewHandler(c, caller, log)` takes
  a required `CallerAuthenticator`; a nil caller becomes a deny-all (fail closed),
  so a Handler is never accidentally open. The bearer and chain authenticators are
  removed. The remote client has a single constructor, `NewHTTP(baseURL, client,
  source)`, that presents a token from `source` on every request — the projected
  ServiceAccount token the kubelet rotates in place.
- **Both tiers require their identity.** The orchestrator requires
  `CONTROL_PLANE_AUDIENCE` and authenticates via `internal/controlauth`
  (TokenReview) only. The web tier requires `CONTROL_PLANE_TOKEN_PATH` (its
  projected token) to build the remote client. Missing either is a fail-closed
  startup/`buildControlClient` error.
- **`internal/config` drops `ControlPlaneToken`/`CONTROL_PLANE_TOKEN`.**
- **Per-service identity is the base default.** The web Deployment mounts a
  projected ServiceAccount token (audience `zumble-zay-orchestrator`), the base
  ConfigMap sets `CONTROL_PLANE_AUDIENCE`/`CONTROL_PLANE_CALLERS`, and the
  `system:auth-delegator` binding is active in every environment — not a dev-only
  opt-in. The dev overlay and the `make dev-up` secret bootstrap no longer wire or
  seed anything control-plane-auth-related.

## Consequences

- **No shared control-plane secret exists anywhere.** A leaked ConfigMap, Secret,
  or process environment no longer grants control-API access. Each tier presents
  its own kubelet-rotated, audience-bound identity, and TokenReview + the caller
  allowlist decide every request.
- **Fail closed by construction.** No audience → the orchestrator refuses to
  start; no token path → the web tier refuses to build a control client; a nil
  authenticator → deny all. There is no configuration in which the control API
  authenticates with a static secret, and none in which it runs open.
- **The split control plane is now Kubernetes-only.** TokenReview requires an
  apiserver, so the remote control plane only runs on Kubernetes. This is an
  accepted narrowing: the split deployment always targets Kubernetes, and the
  co-located (in-process `Local`) path — used by `go run`, tests, and CI — makes
  no control-API HTTP call and is unaffected.
- **Tests inject a stub authenticator.** The control-plane tests exercise the HTTP
  plumbing behind a stub `CallerAuthenticator`; the real TokenReview logic is
  tested in `internal/controlauth` against a fake reviewer. No apiserver is needed
  in unit tests.
- **Scope.** This closes the shared-secret gap on the control plane. In-transit
  encryption and within-plane replay of a stolen token remain the mTLS follow-up
  (see 0032, 0033); they are orthogonal to removing the shared secret.

## Alternatives considered

- **Keep the bearer as a break-glass fallback** — rejected: a standing shared
  secret is precisely what this ADR removes; a break-glass secret reintroduces the
  crown-jewel credential and the masking problem.
- **Validate the token with JWKS/JWT libraries instead of TokenReview** — rejected
  in 0031 and unchanged here: TokenReview needs no JWT dependency, and the
  apiserver verifies the tokens it issued.
- **Defer the retirement** — rejected for this pass: the operator asked to land a
  clean, best-practice per-service identity model as the foundation for the agentic
  identity abstractions, and the fallback's residual value no longer justified the
  standing secret.
