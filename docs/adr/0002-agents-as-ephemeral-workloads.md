# ADR 0002: Agents are ephemeral spawned workloads

Status: Accepted — 2026-06-22

## Context

The agentic plane analyzes multiple sources (a user's GitHub items, their email
and Teams threads, public industry info) and writes ZZ metadata. The runtime
model was clarified over discussion: a monolithic control plane **spawns**
agent runtimes; each agent runtime is an integral operational unit with a
bounded lifecycle. A single runtime is not multi-tenant.

## Decision

Model agents as **ephemeral workload identities**. An orchestrator (control
plane, durable identity) spawns single-purpose runtimes. Each runtime receives a
ZZ-minted, job-scoped, short-lived token. Authorization is computed once at
spawn time as the intersection:

```
orchestrator authority ∩ user standing consent ∩ runtime-type policy
```

The token is self-describing (subject = ephemeral runtime, claims encode the
target user, datapoint, scopes, TTL); ZZ authorizes requests from the token
alone.

## Consequences

- Minimal blast radius: a compromised runtime holds one expiring, single-user,
  single-job credential — nothing durable or cross-user.
- No long-lived secrets in ephemeral units; the vault remains the only holder of
  delegated provider tokens.
- Clean provenance: each ephemeral identity maps 1:1 to a job, so metadata
  writes trace to runtime → job → user → signals.
- Requires a runtime-type registry (capability templates) the mint check reads.
- No need for full multi-tenant impersonation/OBO at request time.

## Amendment (2026-06-23)

- [ADR 0006](0006-credential-broker-not-data-broker.md): runtimes connect to
  providers directly and **transiently hold** a ZZ-vended provider credential
  for the duration of a job (the vault remains the *durable* holder). With the
  GitHub OAuth App that credential is long-lived — an accepted tradeoff, not a
  stopgap: a GitHub App was ruled out because its tokens are installation-scoped
  ([ADR 0013](0013-stay-on-github-oauth-app.md)).
- [ADR 0007](0007-orchestrator-colocated-until-spawn.md): the orchestrator is
  co-located in the ZZ process until it gains real spawn privileges or ZZ scales
  past `replicas: 1`.
