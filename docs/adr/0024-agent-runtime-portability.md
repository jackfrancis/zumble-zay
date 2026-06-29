# ADR 0024: Agent-runtime portability — launcher registry, async dispatch, and token exchange

Status: Accepted — 2026-06-29

Builds on [0009](0009-agent-runtime-contract-boundary.md) (the runtime contract
is the substrate boundary), [0012](0012-kubernetes-native-substrates-swappable.md)
(Kubernetes-native substrates are swappable behind the runtime artifact), and
[0023](0023-orchestrator-own-runtime-and-identity.md) (the orchestrator is its
own runtime and identity).

## Context

We want teammates to experiment with other agent runtimes — kagent,
agent-sandbox, Ray — without rewriting ZZ. The portability boundary already
exists: `orchestrator.Launcher` (how a job is dispatched) plus the runtime
contract every substrate dials back through (ADR 0009). And three launchers
already exercise that boundary (in-process, k8s-job, k8s-pod), so they are the
references — a fourth, hand-written reference is not needed to prove the seam.

Three frictions remained between "we have a seam" and "a teammate can plug in":

1. **Selection was a hardcoded `switch`** in `cmd/orchestrator`, so adding a
   substrate meant editing core wiring.
2. **`Launch` blocked to completion**, pinning a dispatch worker per in-flight
   job and hard-coupling "job done" to "Launch returned" — which a detached
   substrate (a Ray cluster, a kagent service) cannot satisfy: it reports
   completion by watching the workload or via the ingest callback.
3. **Tokens were only delivered at spawn.** A long-lived *service* runtime
   (kagent) is not born per job, so it must *request* a fresh job-scoped token
   per task — a capability ZZ did not expose.

## Decision

Three additive changes; none requires a non-native substrate to exist, and each
is validated by the launchers already present.

### 1. A launcher registry (driver pattern)

`internal/launcher` holds a registry: `Register(name, Factory)`, `Build(cfg, log)`,
`Names()`. It depends only on the `orchestrator.Launcher` seam and config, never
on any substrate. The built-in launchers register themselves in
`cmd/orchestrator`'s init; a new substrate registers from its own package's init
and is activated by a blank import in `cmd/orchestrator` — no selection switch
changes. An unknown name fails fast, listing what is registered.

### 2. Async dispatch, completion decoupled from the worker

`orchestrator.AsyncLauncher` is an optional capability (it embeds `Launcher`):
`Dispatch` creates the workload and returns its `Handle`; `Await` blocks until it
finishes, keyed only off the `Handle` so the orchestrator can call it on its own
goroutine with no shared launcher state. The orchestrator now awaits every job
off the dispatch worker — for an async launcher, `Dispatch` runs on the bounded
worker pool (so concurrent substrate creates stay bounded) and `Await` runs on a
cheap per-job goroutine; for a plain `Launcher`, the whole blocking `Launch` runs
off the worker. Either way a long-running job never pins a dispatch worker, and
completion is no longer hard-coupled to a `Launch` return — the groundwork for a
substrate that reports completion via the ingest callback. The Kubernetes Job and
Pod launchers implement `AsyncLauncher` by splitting their existing create/watch;
`Launch` remains as a composition for direct callers. The orchestrator owns the
await goroutine, so its panic isolation and per-job deadline still apply. (This
also closed a latent race: enqueue now sends and Stop now closes the queue under
the same lock, so a pipeline-chaining send can never hit a closed channel.)

### 3. RFC 8693-flavored token exchange (pull path)

`POST /control/token` on the orchestrator's control API issues a fresh
job-scoped token to an authenticated caller — the pull complement to
push-at-dispatch, for service runtimes that request a token per task.
`orchestrator.MintJobToken` applies the same runtime-type policy as dispatch
(the job type's provider and scopes) and returns the signed token with its
lifetime; it creates no tracked Job, since the caller runs the work itself.
Caller authentication is a seam, `controlplane.CallerAuthenticator`: the default
validates the shared control-plane bearer (the same trust as the trigger
routes), and the endpoint is registered only when token exchange is wired
(`WithTokenExchange`).

## Consequences

- **Plugging in a substrate is now: implement `orchestrator.Launcher` (optionally
  `AsyncLauncher`), `launcher.Register` from your package's init, add one blank
  import in `cmd/orchestrator`.** No change to selection, dispatch, or the
  runtime contract. The three existing launchers are the worked examples;
  kagent, agent-sandbox, and Ray are intentionally left as TODOs behind the seam.
  These reference launchers and the runtime contract are the validated in-cluster
  baseline: a new substrate must not modify or regress them, and any change to a
  shared seam (the interfaces, the `ZZClient`/`ZZ_*` contract, the shared
  runtime-spec helpers, `JobSpec`, the orchestrator's dispatch/completion) must be
  backward-compatible and keep their tests green — identical pipeline output
  across the references is the regression check.
- The dispatch worker pool no longer bounds in-flight jobs — await goroutines do
  the waiting and are cheap; the per-job deadline still bounds them, and `Stop`
  waits for them to drain.
- **Async is groundwork for callback completion.** As shipped here, completion
  arrives by `Await` returning (a watch). Completion via an explicit runtime
  callback — for a fully detached substrate, and as a faster signal for
  Kubernetes — is built on top of this in [ADR 0025](0025-callback-driven-completion.md),
  behind `AsyncLauncher` without changing the interface.
- **Token exchange is real but its caller-auth is a stopgap.** The default trusts
  the shared control-plane bearer, so any holder can mint a token for any user —
  the same authority the control API already has. Per-service identity (validate
  a projected ServiceAccount OIDC `subject_token`) is the hardening, left as a
  `CallerAuthenticator` TODO. The pull path also creates no tracked Job, so the
  UI's "processing" state does not reflect it.
- ZZ is now usable as the authorization server a long-lived runtime integrates
  with (ADR 0001), not only a spawner that injects tokens — the missing piece
  ADR 0009 anticipated for kagent-style substrates.
