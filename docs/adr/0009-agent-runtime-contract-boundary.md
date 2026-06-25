# ADR 0009: The agent runtime contract is the substrate-neutral boundary

Status: Accepted — 2026-06-25

Builds on [0002](0002-agents-as-ephemeral-workloads.md) (agents are ephemeral
workloads), [0006](0006-credential-broker-not-data-broker.md) (ZZ vends
credentials, never proxies data), and [0007](0007-orchestrator-colocated-until-spawn.md)
(orchestrator co-located until it spawns real workloads).

## Context

We want to experiment freely with multiple Kubernetes agentic substrates —
kagent, agent-sandbox, agent-substrate, and custom runtimes — without rewriting
ZZ or coupling it to any one of them. The risk is backing into a corner where
the substrate choice leaks into ZZ core.

Two seams already isolate the substrate: `orchestrator.Launcher` (how a job is
dispatched) and the HTTP calls an agent makes back to ZZ (vend credential, then
ingest). The second was only implicit in `agent.Run`, so its role as the
portability boundary was not visible in the code or recorded as a constraint.

## Decision

**The agent runtime contract — not the launcher, not the language, not the
substrate — is the portability boundary.** A runtime is anything that:

1. holds a **ZZ-minted, job-scoped bearer token**, and
2. speaks two HTTP calls: `POST /agent/credentials/{provider}` (vend) then
   `POST /agent/worklist` (ingest), authenticating with the token over the
   `Authorization` header only.

This contract is expressed in code as `agent.ZZClient`, documented as the ABI
every substrate reimplements. Reinforcing constraints:

- **`orchestrator.Launcher` is the dispatch seam.** In-process today; a kagent /
  agent-sandbox / custom launcher implements the same interface. Token delivery
  (env, projected Secret, CRD field) is each launcher's concern.
- **ZZ core imports no provider client** (ADR 0006); only runtimes do. The
  substrate is purely "a thing that runs token-holding code and dials back to
  ZZ."
- **Runtimes stay thin.** Scoring/synthesis lives in ZZ at ingest (ADR 0008),
  not in the runtime, so swapping substrates never ports business logic.

## Consequences

- Any substrate that can receive a token + job spec and make outbound HTTPS to
  ZZ can host a runtime. kagent (agent-as-a-service), agent-sandbox (one-shot
  Pod), and custom workloads all plug in at the same two seams.
- **Anticipated: a public token mint/exchange endpoint (RFC 8693).** One-shot
  spawn-per-job substrates receive their token at spawn. A long-lived *service*
  runtime (kagent) is not born per job, so it must request a fresh job-scoped
  token per job. Keep token issuance behind the minter seam so exposing this is
  additive, not a refactor.
- **Anticipated: async dispatch/observe in `Launcher`.** `Launch` blocking to
  completion costs one orchestrator goroutine per in-flight job. Completion can
  arrive two ways — `Launch` returns, or the ingest callback lands — so job
  state must not be hard-coupled to only the former.
- **Assumption: runtimes have network egress to ZZ.** A fully network-isolated
  sandbox that forbids egress would invert the contract to a push model (inject
  creds + spec, collect results out-of-band). Default stays the HTTP-callback
  contract; validate egress early when experimenting with isolation.
