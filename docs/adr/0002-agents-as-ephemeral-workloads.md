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
