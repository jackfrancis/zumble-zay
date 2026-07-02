# ADR 0028: Ray / KubeRay agent-runtime substrate

## Status
Proposed

## Context
ADR 0024 makes the agent runtime portable behind the `orchestrator.Launcher`
seam: a substrate registers a factory under a name and is selected by the
`LAUNCHER` env, so adding one touches no orchestrator wiring. ADR 0024 names a
Ray cluster as an intended substrate; this ADR specifies it.

The shipped substrates (`k8s-job`, `k8s-pod`, `agent-sandbox`) all run the
runtime as a **pod they fully define**: they embed the typed
`runtimespec.PodSpec` — the single runtime container plus the `ZZ_*` injection
contract, including the `AI_TOKEN` Secret reference — into a workload object
(`batch/v1` Job, bare Pod, or the Sandbox's `spec.podTemplate`). The controller
runs that pod verbatim, which is why every substrate emits an identical runtime
shape (the cross-substrate non-regression check).

KubeRay does not fit that mold. A `RayJob` runs an **entrypoint on a RayCluster**
(the Ray job-submission model); it has no field that hosts an arbitrary
caller-supplied pod the way the Sandbox's `podTemplate` does. The zumble-zay
runtime is itself a plain, single-process Go batch binary (`cmd/runtime`,
distroless, configured entirely by `ZZ_*` env) — it uses none of Ray's
distributed primitives. So "run the runtime on Ray" is not a 1:1 pod embedding;
it is a deliberate mapping onto the RayJob model.

One job type is the exception. `llm-rank` scores a shortlist with one slow model
(Copilot) call per item — a natural fan-out. The Go `runRank` already fans this
out with bounded goroutine concurrency inside one process; on a Ray cluster the
same fan-out can instead be distributed across the cluster's nodes using **Ray
actors**. So on the ray substrate `llm-rank` runs a Python Ray program instead of
`/runtime` — the substrate's one use of Ray's own compute primitives, specified
below.

## Decision
Add an `internal/raylauncher` substrate, registered as `LAUNCHER=ray`, gated
behind the `ray` build tag (so the default orchestrator binary carries neither
the registration nor the KubeRay/dynamic-client code), mirroring how
`agent-sandbox` is gated and self-registers.

It maps a job onto a **`RayJob` (ray.io/v1) targeting a standing RayCluster via
`clusterSelector`**:

- **Standing cluster, not ephemeral.** zumble-zay jobs are short and frequent
  (often a single LLM call); an embedded `rayClusterSpec` would pay a full
  RayCluster cold-start per job. A standing, autoscaled RayCluster stays warm and
  lets Ray's autoscaler provide the scaling — the actual value of running on Ray.
  The cluster name is configuration (`RAY_CLUSTER`).
- **Reuse the env contract, not a pod spec.** Since no pod can be embedded, the
  launcher renders the same per-job `ZZ_*` environment (built by `agent.Env`,
  the exact map the other substrates inject) into the RayJob's `runtimeEnvYAML`
  `env_vars`, and runs `entrypoint: /runtime`. The runtime binary must therefore
  exist on the RayCluster image.
- **`AI_TOKEN` stays on the cluster, never in `runtimeEnvYAML`.** `runtimeEnvYAML`
  is plaintext in the CR, so the ranking-model token is **not** placed there.
  Instead the standing RayCluster's pods carry `ZZ_AI_TOKEN` from a Secret (set
  once, out of band). This is the one intentional deviation from the other
  substrates' injection contract, and it is a security property, not an
  oversight: a per-job CR never embeds the model secret in clear text. (The
  llm-rank actors path is the single exception: Ray does not reliably propagate a
  cluster-pod env var into actor processes, so it injects the token into that
  job's `runtimeEnvYAML` — see "Intra-job parallelism" below.)
- **Unstructured + dynamic client.** The CR is built `unstructured` and submitted
  with `client-go`'s dynamic client, so the substrate adds **no** new module
  dependency (the typed `kuberay/ray-operator` types and their transitive graph
  stay out of the build), consistent with keeping each substrate's footprint
  isolated (cf. ADR 0027).
- **Lifecycle.** `Dispatch` creates the RayJob; `Await` polls `.status.jobStatus`
  to a terminal state (`SUCCEEDED` → ok; `FAILED`/`STOPPED` → error);
  `shutdownAfterJobFinishes: true` + `ttlSecondsAfterFinished` self-reap, the
  RayJob analogue of a Job's TTL. The runtime also reports completion to ZZ
  (ADR 0024/0025), so the callback races the poll as with the other substrates.

### Intra-job parallelism: `llm-rank` runs Ray actors
For `llm-rank` — and only `llm-rank` — the launcher swaps `spec.entrypoint` to
`python /llm_rank_ray.py` (`entrypointFor`), a Python Ray program that uses genuine
`@ray.remote` **actors** to distribute per-item scoring across the cluster. There
is no toggle; this is the ray substrate's llm-rank behavior. Every other job type
still runs `/runtime`.

- **Same seam, same CR.** Only the entrypoint changes; the RayJob envelope,
  `clusterSelector`, TTL, and the `ZZ_*` `runtimeEnvYAML` contract are identical —
  no new field, no new dependency, no orchestrator change.
- **Same ZZ contract.** The program speaks the exact agent HTTP contract the Go
  runtime uses (`GET`/`POST /agent/worklist`), setting each item's
  `signals.proposed`; ZZ core still ratifies against its deterministic baseline
  (docs/adr/0011).
- **Same image.** `deploy/ray/Dockerfile.ray` bakes `/llm_rank_ray.py` alongside
  `/runtime`, so one image serves both paths.
- **Token to the actors.** Ray's `runtime_env` does not reliably propagate the
  cluster pod's `ZZ_AI_TOKEN` into actor processes, so when a direct model token is
  configured the launcher injects it into this job's `runtimeEnvYAML` — the one
  channel Ray guarantees reaches actors. On the dev overlay the call goes through
  the agentgateway (which holds the credential), so no token is placed on the CR.

`llm_rank_ray.py` does: `ray.init(address="auto")` → `GET /agent/worklist` → spawn
a pool of `@ray.remote class Scorer` actors (`RAY_LLM_RANK_ACTORS_N`, default 4)
that Ray schedules across head/worker nodes → round-robin items across them and
`ray.get()` the proposals in parallel (best-effort per item) → `POST /agent/worklist`
writes back only the items that got a proposal. The actors are **processes inside
the standing head/worker pods**, so scaling the pool adds no pods.

## Configuration
- `RAY_CLUSTER` (required): name of the standing RayCluster (`clusterSelector`
  value `ray.io/cluster`). `build` fails fast if unset, like the other
  in-cluster substrates.
- `RAY_NAMESPACE`: defaults to `cfg.Runtime.Namespace`.
- `RAY_RUNTIME_ENTRYPOINT`: defaults to `/runtime`.
- `RAY_JOB_TTL_SECONDS`: defaults to 300.
- `RAY_LLM_RANK_ACTORS_N`: `llm-rank` actor pool size; defaults to 4.
- `RAY_LLM_RANK_METRICS_LINGER_S`: optional seconds the `llm-rank` job lingers
  after scoring so its short-lived Ray metrics get scraped; empty/0 = no linger.

## Consequences
- The runtime image and the RayCluster image must be reconciled so `/runtime`
  exists where the entrypoint runs. (A future option is a Ray-native wrapper that
  shells to `/runtime` as a task; deferred — it buys nothing for a batch binary.)
- Honest scope: this uses Ray for cluster lifecycle, scheduling, and autoscaling
  of an otherwise-batch workload. Four of five job types run the single-process
  `/runtime` binary — Ray as a *scheduler*. Only `llm-rank` uses Ray's own compute
  primitives (actors, above).
- **The actors path is a demonstration, not a throughput win for this workload.**
  Each item is one slow, I/O-bound model call, and the Go `runRank` already fans
  those out with bounded goroutine concurrency in a single process — which
  saturates a single shared endpoint. Ray actors match that concurrency with more
  machinery (a Python reimplementation, cross-node scheduling); the model API is
  the ceiling either way. Actors only pull ahead when the per-item work is
  CPU/GPU-bound or must scale past one node. The value here is architectural — it
  demonstrates and enables true intra-job Ray parallelism on the substrate, and is
  the reference for jobs where that pays off. docs/adr/0031 is the self-hosting
  direction that makes it pay off.
- Two runtimes now implement llm-rank (Go `/runtime` and the Python actors
  program); they share the ZZ wire contract and the ranking intent, but the logic
  is duplicated across languages — a deliberate, contained cost of the ray
  substrate. The Go `/runtime` remains the implementation every other substrate
  uses.
- Non-goals: no Ray Serve, no long-lived actors across jobs — the actors live only
  for one llm-rank job, then the RayJob self-reaps (`shutdownAfterJobFinishes`).
  Both are the future direction in docs/adr/0031.
- The substrate is build-tag-gated, so it is merge-clean with the default binary
  and with the other substrates: adding it touches only its own package and a
  tagged blank import.

## Observability
The actors path is observable through Ray's standard tooling: the Ray dashboard
(`svc/zz-ray-head-svc:8265`) shows the llm-rank job and its `Scorer` actors spread
across nodes, and Ray's Prometheus metrics expose per-actor series. The repository
documents the **official KubeRay** Prometheus/Grafana install (kube-prometheus-stack
+ KubeRay `PodMonitor`s + Ray's Grafana dashboards) in `deploy/ray/monitoring/`.

The actors program also emits **application metrics** via `ray.util.metrics` —
`ray_zz_items_scored` (counter), `ray_zz_score_errors` (counter, tagged by failure
kind), and `ray_zz_score_latency_seconds` (histogram). Because the actors are
short-lived, `RAY_LLM_RANK_METRICS_LINGER_S` optionally holds the job briefly after
scoring so these land in Prometheus (a Pushgateway is the production answer). This
built-in view of a distributed agent execution is a genuine advantage of the ray
substrate over the pod-based launchers, which surface logs only.

## Validation
Verified end-to-end on a local kind + KubeRay cluster: 4 of 5 job types ran
`/runtime` as RayJobs on the standing cluster, and an `llm-rank` RayJob ran
`python /llm_rank_ray.py`, spawned four `Scorer` actors across the head and worker
nodes, and scored the full worklist via Copilot — `scored 22/22 items via 4 Ray
actors`. The model call goes through the dev overlay's agentgateway
(`ZZ_AI_ENDPOINT`), which holds the provider credential (ADR 0006), so the actors
need no token of their own on the dev path.
