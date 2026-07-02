# ADR 0031: Self-hosted model on Ray Serve (future direction)

## Status
Proposed — future direction, not yet built.

## Context
ADR 0028 runs zumble-zay's agent jobs on a standing Ray cluster, and `llm-rank`
fans its scoring out across Ray **actors**. But that ADR is honest about a ceiling:
the workload is **I/O-bound**. Every model call goes to a *hosted* API (Copilot via
the agentgateway), so each agent spends its time **waiting on the network**, not
computing. The heavy work — token generation on GPUs — happens on someone else's
machine.

Because of that, most of Ray's value is dormant. The Go runtime's goroutine
concurrency already saturates a single remote endpoint, so the actors path matches
it without beating it; Ray's batching, GPU scheduling, object store, and placement
have nothing to bite on. **Ray only wins once the expensive compute lives inside
the cluster.** This ADR records that direction.

## Decision (direction)
Self-host the ranking model **inside the Ray cluster as a Ray Serve deployment**,
and point the existing AI endpoint at it. This moves inference from an external API
onto the cluster's own GPUs, changing the workload from I/O-bound to
**compute-bound** — the one regime where Ray's primitives pay off.

### What it looks like
- A **Ray Serve deployment** hosts an open model (e.g. a small Qwen/Llama) as a set
  of **replica actors** with **continuous batching** and (fractional) GPU. Serve's
  controller manages replicas, autoscaling, and request routing.
- `ZZ_AI_ENDPOINT` is repointed from the agentgateway/Copilot URL to the in-cluster
  Serve endpoint. Because the agentgateway seam already abstracts the model provider
  behind an OpenAI-compatible URL (ADR 0006), this is largely a **deploy + config
  change, not a core rewrite** — the ZZ contract and the agents are unchanged.
- `llm-rank`'s `Scorer` actors call the Serve deployment via a **`ServeHandle`** (an
  in-process Ray call) instead of an outbound HTTP request. Serve **coalesces the
  concurrent requests from all the actors into GPU batches**, and large context can
  ride the shared **object store** zero-copy.

### How it changes the previous (ADR 0028)

```
BEFORE (0028)                          AFTER (0031)
agents ──HTTP──▶ agentgateway          agents ──ServeHandle──▶ Ray Serve replicas
                    │                                              (your GPUs,
                    ▼                                               in-cluster)
             Copilot API (external)
   inference on someone else's GPUs           inference on the cluster's GPUs
```

| Aspect | Before — hosted API (0028) | After — Ray Serve (0031) |
|---|---|---|
| Model runs on | someone else's GPUs | **your GPUs, in-cluster** |
| Workload is | **I/O-bound** (waiting on network) | **compute/GPU-bound** (your hardware) |
| Bottleneck | remote endpoint latency + rate limits | your GPU throughput — tunable via batching/replicas |
| Ray's role | scheduler (+ one demo actors path) | **genuine compute engine**: batching, autoscaling, placement, object store |
| `llm-rank` actors | match goroutines, no real win | **real win** — parallelize GPU-bound calls, amplified by Serve batching |
| Model calls | outbound HTTP per item | in-process `ServeHandle`, batched |
| Cost / control | per-token API cost, external limits | own the hardware; control latency, batching, cost |

### What Ray Serve provides (that a hosted API cannot)
- **Continuous batching** — merges concurrent requests from many agents into GPU
  batches: the single biggest throughput lever for agentic fan-out.
- **Replica autoscaling** — scale model replicas with load.
- **Fractional GPU + multiplexing** — pack several models/adapters per GPU, route
  per request.
- **Co-location** — agents and the model on the same cluster, talking via handles
  and the object store instead of the public internet.

### Complementary step: resident agent actors
Serve replicas **are** long-lived actors — so self-hosting already makes the *model
tier* resident. The natural follow-on is to make the *agent tier* resident too:
dispatch agents as method calls on standing actors instead of a RayJob-per-invocation.
That removes the per-job pod/etcd churn (the K8s-primitive scaling limit) and lets
warm agents call the co-located model via `ServeHandle`. The two tiers together are
the fully-integrated end state: **warm agents calling a warm model, co-scheduled.**

## Consequences
- Requires **GPUs in the cluster** — real cost and ops (weights, deployment,
  autoscaling) the hosted API avoids.
- **Model quality tradeoff**: an open model may rank differently than Copilot. A
  hybrid is possible — self-host the high-volume/cheap paths, keep the hosted API
  for others — since the AI endpoint is per-provider config.
- The core stays put: the OpenAI-compatible contract and the `orchestrator.Launcher`
  seam are unchanged, so this is additive to ADR 0028, not a rewrite of it.

## Non-goals / open questions
- Model choice, GPU sizing, and whether to route some jobs to the hosted API remain
  open.
- This ADR commits to the **direction and rationale**, not an implementation. It
  exists so the actors work in ADR 0028 reads as the on-ramp it is: the substrate
  that makes self-hosting a config change away, at which point Ray's benefits become
  real for this agentic workload.
