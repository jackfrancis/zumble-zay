# ADR 0030: Job-token pull-path — single-use ticket redemption

Status: Accepted — 2026-07-02. Implemented in `internal/orchestrator` (the
`PullTokenLauncher` seam, an in-memory ticket store, `issueTicket`/`RedeemTicket`),
`internal/controlplane` (`RedeemTicket` across `Client`/`Controller`/`Local`/`HTTP`
+ `POST /control/redeem`), `internal/api` (`POST /agent/token`), `internal/agenta2a`
(runtime-side redemption), and `internal/kagent` (opts in via `PullsToken`). Green
across the suite including `-race` and the build-tagged `agent-sandbox`; not yet
live-run.

Builds on [0029](0029-kagent-durable-runtime-substrate.md) (kagent, the substrate
that motivates this), [0024](0024-agent-runtime-portability.md) (the pull
complement to push-at-dispatch; the token-exchange seam), [0025](0025-callback-driven-completion.md)
(the completion callback whose correlation this must preserve), and
[0023](0023-orchestrator-own-runtime-and-identity.md) (the orchestrator is the
sole minter; the web tier only verifies).

## Context

ADR 0029 ships kagent carrying the job token in the A2A `message.metadata` — the
only channel the controller forwards intact. But the kagent controller **persists
task history** (Postgres in dev): a live bearer token in that metadata sits at rest
for its full TTL, readable by anyone with the store. This is unique to a durable,
task-persisting control plane; every other substrate injects env into a pod ZZ
controls, with no third-party persistence.

Three things shaped the fix.

1. **The naive pull is a lateral move — arguably worse.** ADR 0024 already exposes
   `POST /control/token`: a runtime could authenticate with the shared
   control-plane bearer and exchange `{job_type, acting_user}` for a token. But the
   shared bearer is a **mint-anything crown-jewel** — putting it on the standing
   runtime trades a token-at-rest for a broader *standing* capability. Removing the
   token from Postgres only to hand the runtime the keys to the mint is no net gain.
2. **Completion correlates on the token's `jid`.** A kagent job completes via the
   runtime callback (ADR 0025): `POST /agent/complete` carries no job id; the web
   tier reads it from the **token's `jid` claim** (`internal/api/complete.go`
   forwards `Principal.JobID`). The push path works because dispatch mints `jid =
   spec.JobID`. The existing `/control/token` mints an unrelated `exch-…` id, so a
   naive pull would sever correlation and every kagent job would fall back to its
   deadline backstop instead of completing promptly.
3. **The hops are cluster-internal HTTP.** The runtime↔web-tier and
   web-tier↔orchestrator calls are plain HTTP on the cluster network today; the job
   token already rides them (vend, ingest, complete).

## Decision

Carry a **single-use, job-bound redemption ticket** in place of the token, and
have the runtime exchange it for the job token before it runs.

- **`PullTokenLauncher` seam, opt-in.** A launcher that implements `PullsToken()
  bool` tells the orchestrator to hand it a **ticket** in the `token` argument of
  `Dispatch`/`Launch` rather than the job token. Only kagent opts in; every other
  launcher takes the unchanged push path (`o.minter.Mint`). The choice is one
  `dispatchCredential` call — no launcher but kagent changes.
- **The ticket is a capability, minted at dispatch.** `issueTicket` mints a
  high-entropy (256-bit, `crypto/rand`) opaque id into an in-memory store in the
  orchestrator, bound to `{jobID, jobType, actingUser}` with a short TTL (5 min — it
  need only cover dispatch-to-redeem, seconds for a warm agent). It is **single-use**:
  redemption consumes it. There is no stateless signed-envelope variant — single-use
  requires state, so the store *is* the mechanism.
- **Redemption preserves correlation.** The runtime `POST`s the ticket to the web
  tier's `/agent/token`; the web tier proxies via `controlplane.Client.RedeemTicket`
  to the orchestrator's `POST /control/redeem`; `RedeemTicket` consumes the ticket
  and mints the job token **with `jid = ticket.jobID`** — identical to what dispatch
  would have pushed — so the completion callback still correlates. The path mirrors
  `/agent/complete` exactly (a runtime-facing `/agent/*` route the web tier forwards
  to the orchestrator's control API), so runtimes still talk only to the web tier and
  the control API stays unexposed.
- **Possession of the ticket is the authorization.** Because the ticket is
  single-use, job-bound, and unguessable, **no standing secret rides on the
  runtime**: `/agent/token` is not behind the job-token auth (the runtime has no
  token yet), and the web→orchestrator hop reuses the existing shared control bearer
  (the web tier is its only caller). A leaked ticket is inert once the legitimate
  runtime redeems it (seconds after dispatch) and only ever mints one job's token.
- **One decoder, one contract key.** `ZZ_JOB_TICKET` is an injection-contract key
  *outside* the `Env`/`ParamsFromEnv` pair (like `ZZ_AI_TOKEN`). The runtime
  (`agenta2a`) redeems it, shadows `ZZ_JOB_TOKEN` with the redeemed token, then calls
  `agent.ParamsFromEnv` **unchanged** — so there is still exactly one credential path
  from there on and the encode/decode halves cannot drift.

## Consequences

- **The live token never persists in a substrate's store.** kagent's task history
  holds only an inert, single-use, job-scoped ticket; the usable credential is minted
  on demand and never written down. A real improvement over both the token-in-metadata
  original (ADR 0029) and the shared-bearer pull (which would have relocated the mint
  capability onto the runtime).
- **No new config, secret, or manifest.** The runtime redeems at its existing
  `ZZ_BASE_URL`; the web→orchestrator hop uses the existing `CONTROL_PLANE_TOKEN` /
  `CONTROL_PLANE_URL`. Nothing new is injected into the durable agent.
- **The push path is untouched; the seam is additive.** Only kagent implements
  `PullsToken()`; the reference launchers (`inprocess`, `k8s-job`, `k8s-pod`,
  `k8s-pod-detached`), `opensandbox`, and the build-tagged `agent-sandbox` all mint
  and inject the token exactly as before — verified green, including `-race` and the
  tagged build. A future durable/persisting substrate opts in the same way.
- **Remaining exposure is transport and caller identity, not a token at rest.** The
  redemption hop is cluster-internal HTTP today, like every `/agent/*` call, so mTLS
  (a service mesh) / `NetworkPolicy` on the runtime-facing plane and a per-service
  OIDC identity for the web→orchestrator redeem (ADR 0024) are the follow-ups.
  Single-use + short-TTL already make plaintext capture far less useful than an
  intercepted bearer token — inert after first redeem, non-replayable.
- **The in-memory ticket store follows the in-memory-first rule (ADR 0005).** It is
  correct at `replicas: 1`; a shared ticket store is a prerequisite for scaling the
  orchestrator past one replica, alongside the shared session and worklist stores.
- **Supersedes the credential-carrying decision of ADR 0029** (the job token in
  `message.metadata`); the rest of 0029 stands.
