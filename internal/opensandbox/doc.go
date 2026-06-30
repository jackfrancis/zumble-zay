// Package opensandbox is an agent-runtime substrate that launches each job as an
// OpenSandbox sandbox (https://github.com/opensandbox-group/OpenSandbox, see
// docs/adr/0027). Unlike the in-cluster substrates, OpenSandbox is a remote
// control plane: this launcher is an HTTP client of an OpenSandbox lifecycle
// server (reached at OPENSANDBOX_ENDPOINT with an OPENSANDBOX_API_KEY), which in
// turn schedules the workload on its own Docker or Kubernetes runtime with strong
// isolation. It therefore needs no Kubernetes client and no ZZ RBAC.
//
// The client is hand-rolled over net/http against the OpenSandbox lifecycle and
// execd APIs, so the substrate adds no third-party module to ZZ and needs no build
// tag — it self-registers (LAUNCHER=opensandbox) like the in-cluster launchers.
//
// OpenSandbox sandboxes are long-lived, exec-into environments, not one-shot
// workloads, so the runtime is not the container entrypoint. Dispatch creates a
// keep-alive sandbox (tail -f /dev/null), waits for it to reach Running, then
// execs /runtime into it through execd, carrying the ZZ_* injection contract as
// the command environment. The sandbox image must therefore be shell-bearing and
// contain /runtime (the distroless ZZ runtime image is neither — set
// OPENSANDBOX_RUNTIME_IMAGE). Completion is detached and callback-driven
// (docs/adr/0025): the runtime reports completion to ZZ, the orchestrator races
// that against this launcher's deadline backstop, and the sandbox self-reaps via
// its create-time timeout.
package opensandbox
