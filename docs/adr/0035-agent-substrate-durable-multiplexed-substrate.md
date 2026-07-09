# ADR 0035: Agent Substrate as a durable, multiplexed (suspend/resume) runtime substrate

Status: Accepted — 2026-07-09. Implemented in `internal/substrate` (the launcher
+ a hand-rolled HTTP client of the Substrate router), reusing the runtime side of
[0029](0029-kagent-durable-runtime-substrate.md) verbatim (`internal/agenta2a` +
`cmd/runtime-a2a` served as the actor), with dev wiring in the `Makefile` +
`deploy/k8s/substrate/`. **EXPERIMENTAL:** like [0027](0027-opensandbox-substrate.md),
this stands up an early, external control plane — Agent Substrate is **v0.0.0**,
its APIs are "almost guaranteed to change," and it is explicitly not for
production. It is a *validation* substrate: the most demanding target for ZZ's
launcher seam and identity model, wired so it can be exercised and so its
constraints are recorded, not a stable production path. Not yet validated
end-to-end against a live `ate-system`.

Builds on [0012](0012-kubernetes-native-substrates-swappable.md) (substrates are
swappable behind `orchestrator.Launcher`), [0024](0024-agent-runtime-portability.md)
(the launcher registry + async dispatch + token exchange),
[0025](0025-callback-driven-completion.md) (detached, callback-driven
completion), and [0029](0029-kagent-durable-runtime-substrate.md) (the durable
BYO-runtime archetype it follows). It refines [0030](0030-job-token-pull-path.md)
(the pull-path ticket) to a substrate whose *memory* — not just a control plane's
task store — is persisted at rest, and stands (like 0029) in deliberate tension
with [0002](0002-agents-as-ephemeral-workloads.md).

## Context

[Agent Substrate](https://github.com/agent-substrate/substrate) ("ate") is a
Google-originated (Apache-2.0) Kubernetes system for running enormous numbers of
agent workloads cheaply. It **multiplexes** many logical **actors** (agent apps)
onto a small pool of warm **worker** pods, and uses gVisor (`runsc`)
checkpoint/restore to **suspend/resume** an actor's *entire process* — RAM and
filesystem — to and from object storage in sub-second time, deliberately keeping
the Kubernetes control plane out of the request hot path. It is the first
substrate ZZ has met that is simultaneously **durable**, **multiplexed**,
**snapshotted**, and **owns its own workload identity**. That combination makes it
the sharpest test of whether ZZ's launcher seam and identity model generalize, and
it introduces three constraints no prior substrate did.

- **Its primitives are split across a CRD plane and a gRPC plane.** `WorkerPool`
  (warm standby pods), `ActorTemplate` (the workload blueprint — containers,
  ports, readiness, and a `snapshotsConfig` object-store bucket; immutable, so it
  is a "golden snapshot"), and `SandboxConfig` are custom resources
  (`ate.dev/v1alpha1`). **Actors are not CRDs** — their lifecycle (create, resume,
  suspend, delete) is a **gRPC** service (`ateapi.Control`) served intra-cluster
  over self-signed TLS. The only Go client is the generated stub in the project's
  `pkg/proto`, and the ergonomic wrapper lives under the project's `internal/`, so
  it is **not importable** by ZZ.

- **Dispatch is plain HTTP through the `atenet-router`, and the router
  auto-resumes.** An inbound request with `Host:
  <actor>.<atespace>.actors.resources.substrate.ate.dev` is matched by the router,
  which — this is the load-bearing fact — **resumes a suspended actor on the
  traffic itself** (its ext_proc calls `ResumeActor` and rewrites the upstream
  authority to the worker) before proxying to the actor's port **80**. So ZZ can
  drive a *standing* actor with nothing but a `net/http` POST: **no gRPC is needed
  on the dispatch path.**

- **The actor's environment is frozen in the golden snapshot.** `ActorTemplate`
  env is captured once and shared by every actor of that template, so it cannot
  carry per-job values (they would be identical for all actors, and fixed at
  snapshot time). An actor even learns its *own* id from a `/run/ate/actor-id`
  bind mount, not env. Per-job parameters must ride the **dispatch request**.

- **A live credential in the actor's RAM is persisted at rest.** Suspend
  serializes process memory to object storage. A job token held in the runtime's
  memory would therefore sit at rest across a suspend/resume — a *stronger* version
  of 0029's "durable control plane persists task history" concern, because here it
  is the runtime's own address space, not a proxy's store.

## Decision

Ship Agent Substrate as an **untagged, self-registering substrate** in the
**durable-runtime archetype of ADR 0029**: ZZ dispatches each job over HTTP to a
**standing actor** provisioned out-of-band, carries per-job params (and a
redemption ticket in place of the token) in the request the router forwards, and
completes via the runtime callback. The novel constraints are absorbed at the
seam, not in ZZ core.

- **Own package, own activation, no new module, no gRPC on the MVP path.**
  `internal/substrate` implements `orchestrator.Launcher` + `AsyncLauncher` +
  `PullTokenLauncher`, registers itself (`launcher.Register("substrate", …)`) from
  a package `init`, and is activated by its *own* blank-import file in
  `cmd/orchestrator`. The client is a thin hand-rolled `net/http` caller that POSTs
  through the `atenet-router` with the actor `Host` header; it pulls no
  third-party module, so `go.mod`/`go.sum` are untouched and **no build tag is
  needed** (cf. 0027/0029; a tag isolates a heavy module, and the actor-lifecycle
  gRPC — the only thing that would pull one — is deliberately *not* on this path).
  Router endpoint + actor coordinates are read in `build()` from `SUBSTRATE_*`
  env, so `internal/config` is untouched. Select with `LAUNCHER=substrate`.

- **The actor IS the ADR 0029 runtime, reused verbatim.** Because the router is a
  transparent HTTP proxy and A2A is just HTTP+JSON, the standing actor runs the
  **exact `cmd/runtime-a2a` image** — the same `internal/agenta2a` server that
  hosts `agent.Run`, decodes per-job params **metadata-first-then-env** through the
  one `agent.ParamsFromEnv` decoder, redeems the ticket, and reports completion.
  ZZ's substrate client sends the **same A2A `message/send`** body the kagent
  client sends; only the transport differs (router + `Host` header instead of the
  kagent controller's `/api/a2a/{ns}/{name}/` path). No new runtime binary, no new
  Dockerfile, no second params decoder — the cross-substrate non-regression
  invariant (0012) holds by construction.

- **Per-job in the dispatch, static in the golden snapshot.** The launcher puts
  only per-job keys (`ZZ_JOB_TYPE`, `ZZ_PROVIDER`/`ZZ_ITEM_ID` when set, and
  `ZZ_JOB_TICKET`) in the A2A message metadata — the carrier the router forwards
  to the actor. Static config (`ZZ_BASE_URL`, `ZZ_AI_*`) lives on the
  `ActorTemplate` env, i.e. *inside* the golden snapshot, which is exactly where
  frozen-and-shared values belong. This is the same split as 0029; here the
  env-is-frozen property makes the split mandatory rather than merely tidy.

- **A ticket, never the token — because the runtime's RAM is snapshotted.**
  `PullsToken` is true (0030): the orchestrator hands `Dispatch` a **single-use,
  short-TTL, job-bound redemption ticket**, and the runtime exchanges it at `POST
  /agent/token` for the job token before running. This keeps a live bearer token
  out of any snapshot of the actor's memory; the only window in which a token
  exists in RAM is the active run, bounded by the job deadline. The redeemed token
  carries the dispatched job's id, so the completion callback still correlates.

- **Detached run + non-blocking send + callback completion (ADR 0025).**
  Identical to 0029 and unchanged in the reused runtime: the A2A server sets
  `ReportCompletion=true`, runs `agent.Run` on a background context bounded by
  `JobTimeout`, and acknowledges immediately; the launcher is an `AsyncLauncher`
  whose `Await` waits the per-job deadline while the real outcome arrives via `POST
  /agent/complete`. Completion is decoupled from any proxy/idle timeout the router
  or a suspend might impose.

- **The actor is provisioned out-of-band; ZZ never touches the actor gRPC.** The
  `WorkerPool` + `ActorTemplate` are applied as manifests
  (`deploy/k8s/substrate/`), and the standing actor is created with the project's
  `kubectl-ate` plugin in the install step — the same shape as 0029's BYO `Agent`
  CR applied before dispatch. ZZ holds **no pod/job-create RBAC** on this path (it
  is a client of Substrate's control plane, as with 0027/0029) and needs no gRPC
  stub. Dynamic actor management (create/suspend/delete per job via `ateapi.Control`)
  is a deliberate **non-goal** for the MVP: it would pull grpc-go + protobuf + the
  generated stubs and fight the "keep the control plane out of the hot path"
  design; if ever wanted it belongs behind a build tag (the 0026 pattern), in its
  own file, never on the default path.

- **The actor binds `:80`, root-in-sandbox.** The router targets the worker's port
  80, and the reused `runtime-a2a` image is distroless-**nonroot** (listening on
  `:8080` by default), so the `ActorTemplate` sets `PORT=80` and runs the container
  as uid 0. Binding a privileged port as root is acceptable here precisely because
  the actor runs inside a gVisor sandbox — strong isolation is Substrate's whole
  premise — so root-in-sandbox is not root-on-node. This is a per-substrate
  manifest choice; the shared image and its Dockerfile are untouched.

## Consequences

- **Default build untouched; references stay green; no seam change.** No new
  module, no build tag, no change to `Launcher`/`AsyncLauncher`/`PullTokenLauncher`,
  `ZZClient`, the `ZZ_*` contract, `runtimespec`, or the dispatch/completion path.
  The reference launchers and `agent-sandbox`/`opensandbox`/`kagent` are
  unaffected. This substrate reuses 0029's runtime whole, so it adds *only* a
  launcher + client + dev wiring.

- **The env-frozen and RAM-snapshot constraints are absorbed without leaking into
  core.** The two genuinely new properties are handled entirely at the seam:
  per-job params ride the dispatch (already true since 0029), and the credential is
  a pull-ticket (already true since 0030). That both pre-existing decisions
  *happened* to be exactly what a snapshotted, env-frozen substrate needs is the
  strongest evidence this session set out to gather — the abstractions generalize.

- **A new operational shape: a multiplexed, suspendable, standing runtime.** There
  is no per-job workload to garbage-collect; the actor stands and may be suspended
  between jobs (its RAM/FS teleported to object storage) and auto-resumed by the
  router on the next dispatch. The suspend-*between*-jobs window is the sweet spot
  — ZZ runs a job to completion, then goes idle — so a suspend never freezes an
  in-flight outbound call to GitHub or the model.

- **Activation prerequisites are heavy, and the cluster itself is special (the
  EXPERIMENTAL cost).** Unlike every other substrate, Substrate cannot run on ZZ's
  ordinary dev cluster: its `podcertcontroller` (the mTLS-identity signer the whole
  `ate-system` depends on) needs **create-time** apiserver feature gates —
  `ClusterTrustBundle`, `ClusterTrustBundleProjection`, `PodCertificateRequest`,
  plus the `certificates.k8s.io/v1beta1` `runtimeConfig`, on a ≥1.36 node image —
  that cannot be added to a running cluster. And `ate-system` has **no published
  images** (every component, including the `ateom` herder a `WorkerPool` names, is
  `ko://`-built from source), so it cannot be `kubectl apply`-ed from a URL like the
  agent-sandbox/kube-network-policies installs. Both facts force the bring-up to be
  "clone Substrate and drive its own gated-kind + `ko` installer." So
  `LAUNCHER=substrate make dev-up` resolves `cluster-up` to a `substrate-cluster`
  target that **deletes and recreates** the kind cluster via Substrate's
  `hack/create-kind-cluster.sh` (feature gates, local registry, gVisor, proxy-ARP)
  + `hack/install-ate-kind.sh --deploy-ate-system` (control plane, `rustfs`/`valkey`,
  `atelet`); the dev-up substrate branch then pushes the `runtime-a2a` actor image
  to that registry as a digest, `ko`-resolves the `ateom` herder, and applies the
  pool/template + atespace/actor against the in-cluster `rustfs` snapshot bucket. It
  requires `docker` (Substrate's scripts assume it), `go`, and `git`, is pinned to a
  moving v0.0.0 ref, and is expected to need per-cluster tuning (as 0027 is).

- **The north-star follow-up is identity composition.** Substrate ships a native
  **`SessionIdentity`** service (`MintJWT` — an OIDC JWT identifying the actor;
  `MintCert` — mTLS via CSR). The MVP treats Substrate as dumb compute and injects
  a ZZ pull-ticket. The deeper integration — the actual test of "generalize
  agentic identity for app developers" — is for ZZ to **validate a Substrate-minted
  `SessionIdentity` JWT** at `/agent/*` through the existing `authn.TokenValidator`
  seam, i.e. ZZ becomes a resource server that trusts the substrate's own IdP
  rather than injecting a credential at all. That, plus the runtime→web `/agent/*`
  transport hardening (mTLS/`NetworkPolicy`) already outstanding from 0029/0034, is
  the substantive next increment; this ADR deliberately scopes to the launcher so
  that experiment has a working substrate to run against.
