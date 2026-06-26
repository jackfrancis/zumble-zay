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
- **Known limitation / tracked improvement:** the first slice keeps the existing
  GitHub **OAuth App**, whose user tokens are long-lived and cannot be
  downscoped, so a compromised runtime holds a long-lived user token for its job
  window. Planned improvement: migrate GitHub to a **GitHub App** issuing
  expiring user-to-server tokens (refresh tokens held in the vault) so each job
  receives a genuinely short-lived credential. **Prioritize before agents run as
  out-of-process workloads or before onboarding untrusted agent code.**
  - *In progress:* the **refresh-on-vend** mechanism is implemented — vend reads
    the vault and, when the stored credential has a refresh token and has
    expired, refreshes via the provider's OAuth token endpoint, persists the
    rotated pair, and vends the fresh access token (`auth.Handler.Credential`
    behind the `api.CredentialSource` seam). It is gated on a refresh token
    being present, so it is a no-op for the current OAuth App and activates
    automatically once a **GitHub App** (with user-token expiration enabled) is
    registered and its client ID/secret configured — no further code change is
    required, since the OAuth flow is identical and GitHub Apps ignore classic
    scopes.
- Amends ADR 0001 (ZZ vends credentials; it does not proxy data) and ADR 0002
  (the vault is the *durable* holder; runtimes are *transient* holders for the
  duration of a job).
