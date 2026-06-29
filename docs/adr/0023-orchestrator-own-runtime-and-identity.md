# ADR 0023: Extract the orchestrator into its own runtime and identity

Status: Accepted — 2026-06-29

Realizes the extraction trigger in [0007](0007-orchestrator-colocated-until-spawn.md)
(orchestrator co-located until it spawns real workloads). Builds on
[0001](0001-zz-as-authorization-server.md) (ZZ is the authorization server),
[0002](0002-agents-as-ephemeral-workloads.md) (agents are ephemeral workloads),
and [0009](0009-agent-runtime-contract-boundary.md) (the runtime contract is the
substrate boundary).

## Context

ADR 0007 kept the orchestrator a separate package but physically co-located in
the ZZ process, to be extracted when *either* (a) it gained real
workload-spawning privileges, or (b) ZZ scaled past `replicas: 1`. Trigger (a)
is now met: the deployed default is `LAUNCHER=k8s-job`, and the web tier's
ServiceAccount is bound to a Role that can `create/delete` Jobs and Pods. So the
internet-facing process holds namespaced Pod/Job-creation RBAC — exactly the
coupling ADR 0007 was written to prevent. The web binary also linked the full
Kubernetes client (~132 packages) solely to launch jobs.

Separately, the orchestrator had no identity of its own. It minted job tokens
with an HMAC key derived from `SESSION_SECRET` — the same root secret that signs
session cookies — and the same instance both minted and validated. "Orchestrator
authority" in the RFC 8693 intersection (`orchestrator ∩ user consent ∩
runtime-type policy`) was therefore vacuous.

## Decision

Extract the orchestrator into its own deployable with its own identity, while
keeping a co-located mode for single-process runs.

1. **Separate binary and Deployment.** A new `cmd/orchestrator` binary runs the
   control plane: it owns the launcher (in-process / k8s-job / k8s-pod), mints
   job tokens, and serves a small control API. It is the only workload with
   Pod/Job-creation RBAC, bound to a dedicated `zumble-zay-orchestrator`
   ServiceAccount; the web tier's ServiceAccount loses that RBAC. `cmd/server`
   no longer imports the launcher or the Kubernetes client (0 client-go
   packages, down from 132).

2. **Internal HTTP control API.** The web tier reaches the orchestrator through
   `controlplane.Client`, served two ways behind one interface: `Local` calls a
   co-located orchestrator directly (the single-process default, used by tests,
   CI, and local dev), and `HTTP` calls the orchestrator's control API
   (`POST /control/ingest|converse|research`, `GET /control/active`) over the
   cluster network. The API is authenticated with a shared bearer
   (`CONTROL_PLANE_TOKEN`), constant-time checked, fail-closed: it triggers
   privileged spawns even though it is cluster-internal (ClusterIP, never
   exposed through the Ingress). The reverse direction — runtimes calling back
   to `/agent/*` — was already network-shaped (ADR 0009), so it is unchanged.

3. **Asymmetric job-token signing.** Job tokens move from HMAC to Ed25519. The
   orchestrator holds the private key and is the **sole issuer** (`mint.Minter`);
   the web tier holds only the public key and **verifies** (`mint.Verifier`,
   which implements `authn.TokenValidator`). The web tier can authenticate a
   runtime's bearer but can never mint one — making "the orchestrator is the
   authorization server" (ADR 0001) real in code, not just in topology.

4. **Staged, not a full cutover.** The `Local` adapter and the in-process
   launcher keep single-process mode fast and zero-config for tests, CI, and
   local dev; the split (two Deployments, two identities) is what the cluster
   overlay deploys. `Local` is deliberately deletable once the HTTP path is the
   only one in use — a small mechanical follow-up, not a redesign.

The staleness reconciler stays in the web tier for now: it reads the in-memory
worklist store, which lives there. It enqueues research through the same
`controlplane.Client`. It moves to the orchestrator once the store is shared
(persistence; see the roadmap), at which point a single leader is needed past
`replicas: 1`.

## Consequences

- The internet-facing process holds no cluster-write capability and no
  Kubernetes client. A web-tier compromise cannot create workloads directly; it
  can only ask the orchestrator for the same per-user operations it could
  already perform via the vault.
- The orchestrator is the single token issuer; key compromise is scoped to one
  non-internet-facing process, and the web tier needs only a public key.
- One more cluster-internal trust boundary (web → orchestrator) exists, secured
  by a shared bearer over ClusterIP. mTLS or a NetworkPolicy can tighten it
  later without changing the seam.
- **Key distribution is staged.** By default both tiers derive the Ed25519
  keypair from `SESSION_SECRET` (simple, zero-config, but the web tier could
  re-derive the private key). True issuer/verifier separation is a config step:
  provision an independent `MINT_PRIVATE_KEY` to the orchestrator only and
  `MINT_PUBLIC_KEY` to the web tier only.
- The orchestrator still requires `SESSION_SECRET` to pass shared config
  validation even when it uses an explicit signing key; relaxing that is a minor
  later cleanup.
- **Anticipated (ADR 0009): a public RFC 8693 mint/exchange endpoint.** With the
  orchestrator now a standalone, identity-bearing control plane, exposing token
  exchange for long-lived *service* runtimes (kagent and similar) has a natural
  home here, behind the same minter seam.
