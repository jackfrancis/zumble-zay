# ADR 0031: Ray-actors execution path for llm-rank (intra-job parallelism)

## Status
Proposed

## Context
ADR 0028 added the `ray` substrate: each agent job runs as a KubeRay `RayJob`
whose entrypoint is `/runtime` — the same single-process Go batch binary every
substrate runs. That path uses Ray purely as a **scheduler/substrate**: it does
not use Ray's own compute primitives (tasks/actors), and a single job is not
parallelized across the cluster. ADR 0028 called this out explicitly as a
non-goal ("it does not (yet) parallelize a single job with Ray tasks/actors").

The `llm-rank` job is the natural candidate for intra-job parallelism: it scores
a shortlist of work items, one slow LLM (Copilot) call per item. The Go
`runRank` already fans this out with bounded goroutine concurrency inside one
process; on a Ray cluster the same fan-out can instead be distributed across the
cluster's nodes using **Ray actors**.

## Decision
On the `ray` substrate, run the `llm-rank` job as a Python Ray program
(`deploy/ray/llm_rank_ray.py`) as the RayJob entrypoint instead of `/runtime`,
using genuine `@ray.remote` **actors** to parallelize per-item scoring across the
RayCluster.

- **Default for llm-rank, scoped to llm-rank.** On the ray substrate an `llm-rank`
  job always runs the actors program; every other job type still runs `/runtime`
  (the ADR 0028 behavior, unchanged). There is no toggle — the actors program *is*
  the ray substrate's llm-rank behavior.
- **Same seam, same CR.** The launcher only swaps `spec.entrypoint`
  (`entrypointFor`); the RayJob envelope, `clusterSelector`, TTL, and the `ZZ_*`
  `runtimeEnvYAML` injection contract are identical to ADR 0028. No new field, no
  new dependency, no orchestrator change.
- **Same ZZ contract.** The Python program speaks the exact agent HTTP contract
  the Go runtime uses (`GET`/`POST /agent/worklist`), setting each item's
  `signals.proposed`. ZZ core is untouched and still ratifies the proposal
  against its deterministic baseline (docs/adr/0011).
- **Token to the actors.** Ray's `runtime_env` does not reliably propagate the
  cluster pod's `ZZ_AI_TOKEN` into actor processes, so when a direct model token is
  configured the launcher injects it into the `llm-rank` RayJob's `runtimeEnvYAML`
  — the one channel Ray guarantees reaches actors. On the dev overlay the model
  call goes through the agentgateway (which holds the credential), so no token is
  placed on the CR at all (see Validation).
- **Same image.** `deploy/ray/Dockerfile.ray` bakes `/llm_rank_ray.py` alongside
  `/runtime`, so one Ray image serves both paths (the Ray base already has
  Python + Ray).

### How the actors work
`llm_rank_ray.py`:
1. `ray.init(address="auto")` — joins the standing RayCluster it runs on.
2. `GET /agent/worklist` — reads the shortlist.
3. Spawns a pool of `@ray.remote class Scorer` actors (count via
   `RAY_LLM_RANK_ACTORS_N`, default 4). Ray schedules them across head/worker
   nodes.
4. Round-robins items across actors, `ray.get()` gathers the `AxisProposal`s in
   parallel — best-effort per item, mirroring the Go runtime.
5. `POST /agent/worklist` — writes back only the items that got a proposal.

## Consequences
- This is the first path that uses Ray for what it is designed for — distributing
  a job's work across the cluster — rather than only as a batch scheduler. It is
  the reference for extending the same treatment to other fan-out jobs
  (e.g. github-enrich) later.
- Two runtimes now implement llm-rank (Go `/runtime` and the Python actors
  program). They share the ZZ wire contract and the ranking prompt intent, but the
  prompt/logic is duplicated across languages — a deliberate, contained cost of the
  ray substrate's llm-rank path. The Go `/runtime` remains the implementation every
  other substrate uses, and the tested default off the ray substrate.
- **This is a demonstration, not a throughput win for this workload.** Each item is
  one slow, I/O-bound model call, and the Go `runRank` already fans those out with
  bounded goroutine concurrency inside a single process — for an I/O-bound,
  single-endpoint workload that already saturates. Ray actors match that
  concurrency with more machinery (a Python reimplementation, cross-node
  scheduling); the model API is the ceiling either way. Actors only pull ahead when
  the per-item work is CPU/GPU-bound or must scale past one node. The value here is
  architectural — it demonstrates and enables true intra-job Ray parallelism on the
  substrate, and is the reference for jobs where that actually pays off.
- Non-goals unchanged: no Ray Serve, no long-lived actors across jobs — the
  actors live only for the duration of one llm-rank job, then the RayJob
  self-reaps (`shutdownAfterJobFinishes`).

## Observability
The actors path is observable through Ray's standard tooling (no bespoke
instrumentation): the Ray dashboard (`svc/zz-ray-head-svc:8265`) shows the job
and its `Scorer` actors, and Ray's Prometheus metrics (`:8080/metrics`) expose
per-actor series such as `ray::Scorer.score` under `ray_component_*`. The
repository documents the **official KubeRay** Prometheus/Grafana install
(kube-prometheus-stack + KubeRay `PodMonitor`s + Ray's Grafana dashboards) in
`deploy/ray/monitoring/README.md`, rather than a hand-rolled Prometheus.

The program also emits **application metrics** via `ray.util.metrics` —
`ray_zz_items_scored` (counter), `ray_zz_score_errors` (counter, tagged by
failure kind), and `ray_zz_score_latency_seconds` (histogram) — exported on the
same endpoint the PodMonitors scrape. Because the actors are short-lived, an
optional `RAY_LLM_RANK_METRICS_LINGER_S` holds the job briefly after scoring so
these land in Prometheus (a Pushgateway is the production answer). Verified live:
`sum(ray_zz_items_scored)` and a p95 of `ray_zz_score_latency_seconds` query
correctly in the official Prometheus.

## Validation
Verified end-to-end on a local kind + KubeRay cluster: an `llm-rank` RayJob ran
`python /llm_rank_ray.py`, spawned four `Scorer` actors across the head and
worker nodes, and scored the full worklist via Copilot — `scored 22/22 items via
4 Ray actors`. The model call goes through the dev overlay's agentgateway
(`ZZ_AI_ENDPOINT`), which holds the provider credential (ADR 0006); the actors
therefore need no token of their own on the dev path.

