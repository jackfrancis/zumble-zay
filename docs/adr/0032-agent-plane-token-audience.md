# ADR 0032: Audience-bind the job token to the agent plane

Status: Accepted — 2026-07-02. Implemented in `internal/mint` (the `aud` claim,
the `AudienceAgent` constant, the `Mint` default, and audience enforcement in the
workload validator) and `internal/authn` (the user plane `RequireAuth` and the
agent plane `RequireScope` are now disjoint by principal kind). Green across the
suite including `-race` and the build-tagged `agent-sandbox`; the change surfaced
and corrected two integration tests that had authenticated on the user `/api/*`
plane with a workload token — the very shortcut this forbids.

Builds on [0031](0031-control-plane-caller-identity.md) (the control plane's
audience binding via TokenReview — this is its token-level analog on the
runtime→web plane), [0009](0009-agent-runtime-contract-boundary.md) (the runtime
contract: a runtime authenticates to ZZ with its job token), and
[0023](0023-orchestrator-own-runtime-and-identity.md) (asymmetric mint: the
orchestrator issues job tokens, the web tier only verifies).

## Context

The runtime→web plane (the `/agent/*` routes) is authenticated per route by the
runtime's ZZ job token (`RequireScope`). Investigating a *per-workload* identity
for this plane — proving the caller is the specific dispatched runtime — surfaced
two facts that reshaped the work.

1. **Per-workload binding does not generalize across substrates.** ZZ controls
   the pod spec only for the `k8s-job`/`k8s-pod`/`k8s-pod-detached` launchers; the
   `agent-sandbox`, `opensandbox`, and `kagent` runtimes are created by their own
   controllers or a remote server, so ZZ cannot inject a per-runtime Kubernetes
   identity into them. A TokenReview approach (as in ADR 0031) would therefore
   cover the reference launchers and **exclude exactly the substrates in active
   use**. A DPoP-style proof-of-possession is substrate-general but injects the
   signing key into the same environment as the token, so it buys little once the
   transport is secured.
2. **The job token had no audience, and the web tier honored any valid workload
   bearer on every `RequireAuth` route.** A token minted for `/agent/*` therefore
   also authenticated on the interactive `/api/*` plane (`/api/me`,
   `/api/worklist`, `/api/thread`). The two planes were not isolated — the token's
   validity was not bound to the plane it was meant for.

So the achievable, substrate-general hardening at the token layer is **audience
binding**, not per-workload binding. Audience binding does not stop replay of a
stolen agent token *within* the agent plane — there is no clean token-layer fix
for that that spans a remote substrate — but that is the province of transport
security (mTLS) plus the short token TTL, tracked with the NetworkPolicy/mTLS
work, not of a workload-identity scheme.

## Decision

Bind the job token to the agent plane with an audience claim, enforce it in the
workload validator, and make the two authentication planes disjoint.

- **Stamp every job token with `aud = zumble-zay-agent`.** `mint.AudienceAgent`
  is defaulted in `Mint`, so no caller — dispatch, ticket redemption, or token
  exchange — changes. The token is now self-describing about the plane it is valid
  for, the token-level analog of ADR 0031's control-plane audience.
- **The workload validator enforces the audience.** `mint.Verifier.Validate` (the
  `authn.TokenValidator`) rejects a token whose audience is not `AudienceAgent`, so
  a token minted for any other audience cannot authenticate a runtime request.
- **Isolate the planes in the middleware.** `RequireAuth` now gates the
  interactive user plane and accepts only an interactive session (`KindUser`); a
  workload token is rejected on `/api/*`. `RequireScope` gates the agent plane and
  accepts only a workload principal (`KindWorkload`) holding the required scope; an
  interactive session is rejected on `/agent/*` — generalizing the `KindWorkload`
  guard the credential-vend handler already carried. The two were decoupled
  (`RequireScope` no longer wraps `RequireAuth`).

## Consequences

- **The planes are disjoint by kind and audience.** A runtime credential minted
  for the agent plane can no longer be replayed on the user API, and a browser
  session can no longer reach the agent sink. Cross-plane confusion — a future
  `/api/*` route reachable by a runtime token, or the reverse — is structurally
  prevented rather than left to per-handler guards.
- **General across every substrate.** The audience rides the job token that every
  substrate already carries; no Kubernetes identity or per-runtime key is required,
  so `agent-sandbox`, `opensandbox`, and `kagent` are covered identically to the
  in-cluster reference launchers. This is why audience binding was chosen over the
  non-general per-workload approaches above.
- **Honest limit.** Audience binding does not stop replay of a stolen agent token
  within the agent plane. Transport security (mTLS) plus the short token TTL is the
  answer there, tracked with the NetworkPolicy/mTLS follow-up.
- **Backward-incompatible at the token layer.** A token minted before this change
  (no `aud`) is rejected by the upgraded web tier. Job tokens are short-lived
  (10 min) and the split tiers deploy together (`replicas: 1`, recreate), so the
  blast radius is a handful of in-flight jobs that retry — the same as any
  token-format change.
- **It caught real misuse.** Two integration tests authenticated on `/api/*` with a
  workload token — the shortcut this forbids. They were corrected to mint a real
  interactive session, which required returning the web tier's session manager from
  the test wiring seam (sessions are server-side, so the cookie must come from the
  handler's own manager). No production behavior beyond the two middlewares changed.
- **No new dependency, config, or manifest.**
