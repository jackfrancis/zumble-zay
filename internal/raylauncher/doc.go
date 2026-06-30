// Package raylauncher is an optional agent-runtime substrate that runs each job
// as a KubeRay RayJob (ray.io/v1, see docs/adr/0028) on a standing RayCluster.
//
// Unlike the Job, Pod, and agent-sandbox substrates — which embed the shared
// runtime PodSpec verbatim — a RayJob runs an entrypoint on a RayCluster and has
// no field that hosts an arbitrary pod. So this substrate reuses the ZZ_*
// injection contract via the RayJob's runtimeEnvYAML (entrypoint: /runtime),
// while the ranking-model token stays on the cluster rather than in the
// plaintext CR. The implementation is gated behind the "ray" build tag, so the
// default build carries none of it; build with `-tags ray` and select it with
// LAUNCHER=ray.
package raylauncher
