# ADR 0005: In-memory stores first, behind interfaces

Status: Accepted — 2026-06-22

## Context

The foundation needs working endpoints without committing yet to a specific
cloud persistence technology. Sessions and the worklist both need storage.

## Decision

Ship in-memory implementations behind interfaces:

- `session` keeps sessions in a process map.
- `worklist.Store` has a `MemoryStore`; the cloud backend will implement the
  same interface.
- `worklist.Ingestor` is a no-op logger until the agentic flow exists.

Because state is per-process, the Kubernetes Deployment runs `replicas: 1`.

## Consequences

- Fast iteration and testable handlers with no external dependencies.
- **Constraint:** horizontal scaling is blocked until a shared session store
  (e.g., Redis) and the cloud worklist store exist. Do not raise `replicas`
  before then — sessions/worklist are not shared across pods.
- Swapping to real backends is an interface implementation, not a handler
  rewrite.
