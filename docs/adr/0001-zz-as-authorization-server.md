# ADR 0001: ZZ is the authorization server

Status: Accepted — 2026-06-22

## Context

External actors (AI agents) must authenticate to ZZ to read user-contextualized
data and write ZZ metadata. ZZ already owns user OAuth consent and will hold an
encrypted vault of delegated provider tokens. The credentials handed to agent
runtimes must be scoped to a specific user and capability set.

Options considered:

1. ZZ issues the scoped tokens itself.
2. A separate corporate IdP (Entra/Okta/Auth0/Keycloak) issues them; ZZ only
   validates.

## Decision

ZZ is the authorization server: it mints short-lived, scoped tokens for agent
runtimes using an OAuth 2.0 Token Exchange (RFC 8693) shape, and it owns consent
and the token vault.

## Consequences

- One place reasons about trust: consent, policy, and minting are co-located; no
  split-brain sync with an external IdP.
- ZZ takes on signing-key management and rotation (a known, bounded cost).
- If a mandated corporate IdP appears later, it can prove *workload identity*
  while ZZ still owns *authorization* — a hybrid, not a rewrite.
