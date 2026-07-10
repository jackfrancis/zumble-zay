// Package metrics is ZZ's Prometheus instrumentation. It is deliberately small:
// a few agent-job histograms (total duration, queue wait, dispatch latency) plus
// the /metrics handler, registered on the default registry so any binary (today
// the orchestrator) can expose it on a listener of its own.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Outcome label values for ObserveJob.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
)

// jobBuckets span the job budgets — a converse turn's ceiling is 15m
// (docs/adr/0019) — so the client default (top bucket ~10s) is replaced with a
// domain-sized set that keeps the tail quantiles meaningful.
var jobBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 120, 300, 600, 900}

// latencyBuckets size the sub-job phases (queue wait, dispatch/provisioning),
// which are seconds not minutes — a warm durable actor resumes in well under a
// second, a cold pod cold-starts in seconds — so they need finer low-end
// resolution than the whole-job buckets.
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}

// countBuckets size small integer counts (model calls, tool invocations per job).
var countBuckets = []float64{0, 1, 2, 3, 4, 5, 6, 8, 10, 15, 20, 30}

// jobDuration is the wall-clock lifetime of an agent job, from enqueue to
// terminal outcome, labelled by job type (github-ingest, github-converse, …), the
// launcher that ran it (inprocess, k8s-job, substrate, …), and outcome. A
// "conversation" is the github-converse job, so filtering this metric to
// type="github-converse" is exactly its total time; the launcher label lets one
// compare substrates across separate runs.
var jobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_duration_seconds",
	Help:      "Wall-clock duration of an agent job from enqueue to terminal outcome, by type, launcher, and outcome.",
	Buckets:   jobBuckets,
}, []string{"type", "launcher", "outcome"})

// queueWait is how long a job sat in the dispatch queue between enqueue and a
// worker picking it up — orchestrator scheduling latency, isolated from the
// launcher and the runtime so a saturated worker pool is visible on its own.
var queueWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_queue_wait_seconds",
	Help:      "Time an agent job waited in the dispatch queue before a worker picked it up, by type.",
	Buckets:   latencyBuckets,
}, []string{"type"})

// dispatchDuration is how long the launcher's Dispatch call took — the
// launcher-attributable provisioning cost, isolated from the model-dominated
// runtime work so launchers stay comparable even under a slow model. For the
// durable/A2A substrates (substrate, kagent) it captures actor resume + routing;
// for the pod launchers it captures the create call (the pod's own cold-start is
// awaited separately, not counted here).
var dispatchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_dispatch_duration_seconds",
	Help:      "Duration of the launcher's Dispatch call (provisioning latency), by launcher and type.",
	Buckets:   latencyBuckets,
}, []string{"launcher", "type"})

// provisioning is the runtime-reported wall time from dispatch-start to the
// runtime actually starting work — the launcher-symmetric startup cost. Where
// dispatchDuration times only the orchestrator's own Dispatch call (and so misses
// a pod launcher's asynchronous cold-start, which happens after Dispatch
// returns), this is measured by the runtime, so a pod's cold-start and a durable
// actor's resume land in one comparable number; provisioning minus dispatch is
// the async boot a pod launcher would otherwise hide (docs/adr/0024).
var provisioning = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_provisioning_seconds",
	Help:      "Runtime-reported wall time from dispatch to the runtime starting work (launcher-symmetric provisioning), by launcher and type.",
	Buckets:   latencyBuckets,
}, []string{"launcher", "type"})

// runtimeWork is the runtime's self-reported in-runtime work time (fetch + model
// loop + write-back) — the job's wall clock minus queue wait, dispatch, and
// completion signalling. Comparing it with job_duration isolates the launcher /
// orchestrator overhead from the model-bound work (docs/adr/0024).
var runtimeWork = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_runtime_seconds",
	Help:      "Runtime-reported in-runtime work time (fetch + model loop + write-back), by launcher and type.",
	Buckets:   jobBuckets,
}, []string{"launcher", "type"})

// modelDuration is the summed wall time a job spent in chat-model calls, split
// out so the model share of a turn is visible against the runtime-work total —
// e.g. a fast small model cuts this without moving the tool/I-O remainder.
var modelDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_model_seconds",
	Help:      "Runtime-reported total chat-model call time for a job, by type.",
	Buckets:   jobBuckets,
}, []string{"type"})

// modelCalls and toolCalls are the loop shape: how many model calls and tool
// invocations a job made. A smaller model that answers with more, cheaper rounds
// shows up here even when per-call latency drops.
var modelCalls = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_model_calls",
	Help:      "Runtime-reported count of chat-model calls per job, by type.",
	Buckets:   countBuckets,
}, []string{"type"})

var toolCalls = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_tool_calls",
	Help:      "Runtime-reported count of tool invocations per job, by type.",
	Buckets:   countBuckets,
}, []string{"type"})

// ObserveJob records one finished agent job. launcher is the substrate that ran
// it (metrics label, from the dispatch Handle); outcome is OutcomeSucceeded or
// OutcomeFailed. It is safe for concurrent use, so the orchestrator can call it
// from whichever goroutine finalized the job.
func ObserveJob(jobType, launcher, outcome string, d time.Duration) {
	jobDuration.WithLabelValues(jobType, launcher, outcome).Observe(d.Seconds())
}

// ObserveQueueWait records how long a job waited in the queue before dispatch.
func ObserveQueueWait(jobType string, d time.Duration) {
	queueWait.WithLabelValues(jobType).Observe(d.Seconds())
}

// ObserveDispatch records how long the launcher's Dispatch call took, by launcher.
func ObserveDispatch(launcher, jobType string, d time.Duration) {
	dispatchDuration.WithLabelValues(launcher, jobType).Observe(d.Seconds())
}

// ObserveProvisioning records the runtime-reported dispatch-to-start latency —
// the launcher-symmetric startup cost (pod cold-start or durable-actor resume).
func ObserveProvisioning(launcher, jobType string, seconds float64) {
	provisioning.WithLabelValues(launcher, jobType).Observe(seconds)
}

// ObserveRuntimeWork records a runtime's self-reported in-runtime work time.
func ObserveRuntimeWork(launcher, jobType string, seconds float64) {
	runtimeWork.WithLabelValues(launcher, jobType).Observe(seconds)
}

// ObserveModelSeconds records the summed chat-model time a job reported.
func ObserveModelSeconds(jobType string, seconds float64) {
	modelDuration.WithLabelValues(jobType).Observe(seconds)
}

// ObserveModelCalls records how many chat-model calls a job reported.
func ObserveModelCalls(jobType string, n int) {
	modelCalls.WithLabelValues(jobType).Observe(float64(n))
}

// ObserveToolCalls records how many tool invocations a job reported.
func ObserveToolCalls(jobType string, n int) {
	toolCalls.WithLabelValues(jobType).Observe(float64(n))
}

// Handler serves the default Prometheus registry — these histograms plus the Go
// runtime and process collectors registered by default. Mount it at /metrics on
// a listener separate from any control or internet-facing port.
func Handler() http.Handler { return promhttp.Handler() }
