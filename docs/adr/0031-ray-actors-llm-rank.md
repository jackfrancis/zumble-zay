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
Add an **opt-in, llm-rank-only** execution path that runs a Python Ray program
(`deploy/ray/llm_rank_ray.py`) as the RayJob entrypoint instead of `/runtime`,
using genuine `@ray.remote` **actors** to parallelize per-item scoring across the
RayCluster.

- **Opt-in toggle.** `RAY_LLM_RANK_ACTORS=true` on the orchestrator turns it on.
  It is scoped to `JobType == llm-rank`; every other job type — and all jobs when
  the flag is off — still runs `/runtime` (the ADR 0028 behavior is the default
  and is unchanged).
- **Same seam, same CR.** The launcher only swaps `spec.entrypoint`
  (`entrypointFor`); the RayJob envelope, `clusterSelector`, TTL, and the `ZZ_*`
  `runtimeEnvYAML` injection contract are identical to ADR 0028. No new field, no
  new dependency, no orchestrator change.
- **Same ZZ contract.** The Python program speaks the exact agent HTTP contract
  the Go runtime uses (`GET`/`POST /agent/worklist`), setting each item's
  `signals.proposed`. ZZ core is untouched and still ratifies the proposal
  against its deterministic baseline (docs/adr/0011).
- **Same token discipline.** The model token is read from `ZZ_AI_TOKEN` carried
  by the **cluster** pods, never placed in the per-job CR (docs/adr/0028).
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
  program). They share the ZZ wire contract and the ranking prompt intent, but
  the prompt/logic is duplicated across languages — a deliberate, contained cost
  paid only on the opt-in path. The Go path remains the tested default.
- For the small local kind RayCluster the parallelism win is modest (few items,
  few nodes); the value is architectural — it demonstrates and enables true
  intra-job Ray parallelism, and scales with worker count on a real cluster.
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

