# ADR 0003: Kubernetes is the runtime substrate

Status: Accepted — 2026-06-22

## Context

The system is a distributed app: a backend, an orchestrator, and many spawned
agent runtimes. We need a substrate that provides workload identity, scaling,
and a consistent dev/prod story. We also want to start simple and evolve.

## Decision

Kubernetes is the substrate. The orchestrator's durable identity starts as a
managed client-secret and evolves to a projected ServiceAccount OIDC token
(federated workload identity) behind an interface seam. Spawned runtimes do not
need a platform identity — they carry ZZ-minted tokens.

Dev mirrors prod via a local kind cluster (`make dev-up`). The container is
distroless/non-root/static and builds identically under Podman or Docker.

## Consequences

- Real, hardened deploy target now; the deployment shape does not change when
  persistence lands (those add backing services, not new topology).
- TLS terminates at the Ingress, enabling `COOKIE_SECURE=true` in prod.
- The orchestrator root identity is the highest-value secret to protect.
- See [0005](0005-in-memory-first.md): `replicas: 1` until shared state exists.
