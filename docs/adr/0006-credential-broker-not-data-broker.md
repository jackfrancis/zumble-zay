# ADR 0006: ZZ is a credential broker, not a data broker

Status: Accepted — 2026-06-23

Amends [0001](0001-zz-as-authorization-server.md) and
[0002](0002-agents-as-ephemeral-workloads.md).

## Context

ADR 0001/0002 and the early architecture described agents reading source data
through ZZ-normalized **signal endpoints** — ZZ would call GitHub / Microsoft
Graph on the agent's behalf and return normalized rows. That makes ZZ a
provider-aware data-plane proxy for every source: it couples ZZ to each
provider's API surface and puts it on the hot path for all retrieval.

We want ZZ to stay a thin authorization + persistence core. Provider
integration logic belongs in the agents that specialize per source.

## Decision

ZZ is a **credential broker, not a data broker**.

- Agent runtimes connect to providers (GitHub, etc.) **directly**. The
  provider-integration logic lives in the agent, never in ZZ.
- ZZ **vends a short-lived provider credential on demand**: a runtime presents
  its ZZ-minted job token to a ZZ credential endpoint and receives the delegated
  credential for its acting user, scoped to the job.
- ZZ **never proxies provider API calls** and never enters the data plane.
- Enforced structurally: **ZZ core packages must not import any provider client
  package; only agent runtimes do.**

## Consequences

- ZZ stays small and provider-agnostic; adding a source (Teams/Graph) is a new
  agent + a vault entry, not new proxy code in ZZ.
- Vend-on-demand is the single point a credential leaves ZZ, so it is the
  natural place to log, scope, and (later) rotate.
- **Blast-radius nuance (amends ADR 0002):** a runtime now *transiently* holds a
  vended provider credential for its job, rather than holding no provider token
  at all. Minimizing that exposure depends on the credential being short-lived.
- **Accepted tradeoff (see [ADR 0013](0013-stay-on-github-oauth-app.md)):** ZZ
  keeps the GitHub **OAuth App**, whose user tokens are long-lived and cannot be
  downscoped, so a compromised runtime transiently holds a long-lived user token
  for its job window. A GitHub App was investigated and **ruled out**: its
  user-to-server tokens are installation-scoped and cannot read the cross-org
  repos the radar depends on. The exposure is bounded instead by keeping the
  token **read-only and public-scoped**, and is mitigated by vault encryption
  (once persisted) and revocation on logout — not by a provider migration.
  - The generic **refresh-on-vend** mechanism is implemented — vend reads the
    vault and, when the stored credential has a refresh token and has expired,
    refreshes via the provider's OAuth token endpoint, persists the rotated
    pair, and vends the fresh access token (`auth.Handler.Credential` behind the
    `api.CredentialSource` seam). It is gated on a refresh token being present,
    so it is a no-op for the GitHub OAuth App and remains useful for any future
    provider (e.g. Microsoft Graph) that issues expiring user tokens.
- Amends ADR 0001 (ZZ vends credentials; it does not proxy data) and ADR 0002
  (the vault is the *durable* holder; runtimes are *transient* holders for the
  duration of a job).
