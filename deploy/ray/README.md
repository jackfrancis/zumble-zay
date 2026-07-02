# Ray / KubeRay launcher

The `ray` launcher runs zumble-zay's agent jobs on a **standing Ray cluster**
instead of a fresh Kubernetes pod per job. It's one of several interchangeable
substrates behind the `orchestrator.Launcher` seam (ADR 0028) — this doc explains
how it works and, honestly, what Ray does and doesn't buy you.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│ namespace: zumble-zay                                            │
│                                                                  │
│   orchestrator ──Dispatch()──▶ RayJob CR ──▶ KubeRay operator    │
│   (LAUNCHER=ray)                              creates a          │
│        ▲                                      submitter pod      │
│        │ Await() polls .status                     │            │
│        │                                           ▼            │
│   ┌────────────── standing RayCluster "zz-ray" (always on) ───┐  │
│   │   zz-ray-head  ·  zz-ray-worker      = Ray's 2 "nodes"    │  │
│   │   the entrypoint runs HERE, on the warm cluster           │  │
│   └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
```

**Per job:** the orchestrator creates **one RayJob** → KubeRay runs **one submitter
pod** → the entrypoint executes on the warm cluster → the orchestrator polls the
RayJob status → it self-reaps (TTL). Same `Launcher` lifecycle as every other
substrate; the work just runs on a standing cluster instead of a new pod.

### The 5 agents and their entrypoint on Ray

| Agent | Job type | Entrypoint | Ray as… |
|---|---|---|---|
| Ingest | `github-ingest` | `/runtime` | scheduler |
| Enrich | `github-enrich` | `/runtime` | scheduler |
| **Rank** | `llm-rank` | **`python /llm_rank_ray.py`** | **compute engine** |
| Converse | `github-converse` | `/runtime` | scheduler |
| Research | `github-research` | `/runtime` | scheduler |

- **4 of 5 jobs run `/runtime`** — the same single-process Go binary every other
  substrate runs. Ray is only a *scheduler* here.
- **`llm-rank` runs the actors program** — `ray.init()`, spawn a pool of
  `@ray.remote Scorer` **actors** across the cluster's nodes, fan the per-item
  scoring across them, gather, write back. This is the one place we use Ray's own
  compute primitives.

**Actors are processes, not pods.** The `Scorer` actors run *inside* the standing
head/worker pods. Scaling the pool (`RAY_LLM_RANK_ACTORS_N`) adds **zero** pods.

---

## What Ray actually provides

### Honest verdict for this workload
zumble-zay calls a **hosted** model API (Copilot). That makes the work
**I/O-bound**: each item is one slow network call, and the process spends its time
*waiting on the endpoint*, not computing. The Go runtime already overlaps those
waits with goroutine concurrency — which fully saturates a single remote endpoint.
So **for llm-rank as it exists, Ray actors do not make it faster**; they reach the
same ceiling (the model API) with more machinery. The current value is *structural
and observational*, not throughput.

### Launcher comparison

| | k8s-job | k8s-pod | agent-sandbox | **ray** |
|---|---|---|---|---|
| Work runs in | fresh pod | fresh pod | sandbox pod | **warm standing cluster** |
| Cold start per job | yes | yes | yes | **no** |
| K8s objects / invocation | 1 | 1 | 1+ | 1 (RayJob) |
| Intra-job fan-out | ✗ | ✗ | ✗ | **✓ actors across nodes** |
| Built-in distributed observability | logs only | logs only | logs only | **✓ dashboard + per-actor metrics** |
| Scaling ceiling | K8s scheduler / etcd | same | same | **Ray (autoscaler, GPU, Serve)** |

**Ray's genuine wins today** (even while I/O-bound):
- **Warm execution** — no per-job pod cold-start; work lands on an already-running cluster.
- **Observable distributed execution** — the Ray dashboard shows the job's actors
  spread across nodes, and custom app metrics (`ray_zz_items_scored`,
  `ray_zz_score_latency_seconds`, `ray_zz_score_errors`) land in Grafana. Pod-based
  launchers give you logs and nothing else.

**What Ray does *not* change today:** pod/etcd churn is the same (still one RayJob
+ submitter pod per invocation), and throughput is unchanged (bound by the remote
model API).

### When Ray genuinely wins — the future direction (ADR 0031)

The benefits unlock when the workload stops being I/O-bound — i.e. when the
**compute moves into the cluster**:

| | Today (hosted API) | Self-host on Ray Serve |
|---|---|---|
| Inference runs on | someone else's GPUs | **your GPUs, in-cluster** |
| Workload is | **I/O-bound** (waiting on network) | **compute-bound** (your hardware) |
| Ray value | scheduler + observability | **continuous batching, fractional GPU, autoscaling, object store, placement** |

Self-hosting flips the workload from I/O-bound to **GPU-bound** — the one regime
where Ray's parallelism, batching, and placement earn their keep. It uses resident
actors automatically (Serve replicas *are* long-lived actors); making the **agents**
resident too (dispatch via Ray method calls instead of a RayJob per invocation)
then removes pod/etcd churn entirely, and lets warm agents call the co-located
model via in-process handles + the shared object store.

---

## Run it

```bash
make dev-up LAUNCHER=ray     # installs KubeRay + a standing RayCluster, wires the orchestrator

# optional knobs
RAY_LLM_RANK_ACTORS_N=8              # Scorer pool size (default 4)
RAY_LLM_RANK_METRICS_LINGER_S=45    # hold the short job open so its metrics get scraped
```

See `monitoring/README.md` for the Prometheus/Grafana setup, and ADR 0028 (the
substrate) / ADR 0031 (the self-hosting future direction) for the design record.
