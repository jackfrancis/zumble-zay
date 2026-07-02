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
  job's `runtimeEnvYAML` — see docs/adr/0031.)
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

## Configuration
- `RAY_CLUSTER` (required): name of the standing RayCluster (`clusterSelector`
  value `ray.io/cluster`). `build` fails fast if unset, like the other
  in-cluster substrates.
- `RAY_NAMESPACE`: defaults to `cfg.Runtime.Namespace`.
- `RAY_RUNTIME_ENTRYPOINT`: defaults to `/runtime`.
- `RAY_JOB_TTL_SECONDS`: defaults to 300.

## Consequences
- The runtime image and the RayCluster image must be reconciled so `/runtime`
  exists where the entrypoint runs. (A future option is a Ray-native wrapper that
  shells to `/runtime` as a task; deferred — it buys nothing for a batch binary.)
- Honest scope: this uses Ray for cluster lifecycle, scheduling, and autoscaling
  of an otherwise-batch workload; the substrate itself does not parallelize a
  single job with Ray tasks/actors. That intra-job fan-out is added for the
  `llm-rank` job by docs/adr/0031 (a Ray-actors entrypoint), which supersedes the
  "(yet)" framing here; every other job type still runs the single-process
  `/runtime` binary.
- The substrate is build-tag-gated, so it is merge-clean with the default binary
  and with the other substrates: adding it touches only its own package and a
  tagged blank import.
