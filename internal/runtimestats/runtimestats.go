// Package runtimestats carries a runtime's self-reported per-job timing across
// the completion callback (docs/adr/0024). It is a dependency-free leaf so every
// tier on the completion path — the runtime that measures it, the web tier that
// receives it, and the orchestrator that turns it into metrics — shares one
// definition without a layering cycle.
package runtimestats

// Timing is a runtime's breakdown of where one job's wall clock went, reported
// on POST /agent/complete. It lets the orchestrator emit per-phase metrics
// without the ephemeral runtime needing its own scraped /metrics endpoint. Every
// field is omitempty, and a zero RuntimeSeconds means "not reported" (the
// in-process path, or an older runtime), so consumers treat zero as absent.
type Timing struct {
	// RuntimeSeconds is the total in-runtime work time (fetch + model loop +
	// write-back): the job's wall clock minus queue wait, dispatch/provisioning,
	// and completion signalling, which the orchestrator measures on its own.
	RuntimeSeconds float64 `json:"runtime_seconds,omitempty"`
	// ProvisioningSeconds is the wall time from the orchestrator dispatching the
	// job to the runtime starting work: a pod's cold-start or a durable actor's
	// resume + routing, measured the same way so launchers are comparable. Zero
	// means not reported (the in-process path, or an older runtime).
	ProvisioningSeconds float64 `json:"provisioning_seconds,omitempty"`
	// ModelSeconds is the summed wall time of the job's chat-model calls.
	ModelSeconds float64 `json:"model_seconds,omitempty"`
	// ModelCalls is how many chat-model calls the job made (one per ranked item,
	// or one per converse tool-loop round).
	ModelCalls int `json:"model_calls,omitempty"`
	// ToolCalls is how many tool invocations the job made (converse only).
	ToolCalls int `json:"tool_calls,omitempty"`
}
