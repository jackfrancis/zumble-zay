// Package metrics is ZZ's Prometheus instrumentation. It is deliberately small:
// a single agent-job duration histogram plus the /metrics handler, registered on
// the default registry so any binary (today the orchestrator) can expose it on a
// listener of its own.
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

// jobDuration is the wall-clock lifetime of an agent job, from enqueue to
// terminal outcome, labelled by job type (github-ingest, github-converse, …) and
// outcome. The buckets span the job budgets — a converse turn's ceiling is 15m
// (docs/adr/0019) — so the client default (top bucket ~10s) is replaced with a
// domain-sized set that keeps the tail quantiles meaningful. A "conversation" is
// the github-converse job, so filtering this metric to type="github-converse" is
// exactly its total time.
var jobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "zz",
	Subsystem: "agent",
	Name:      "job_duration_seconds",
	Help:      "Wall-clock duration of an agent job from enqueue to terminal outcome, by type and outcome.",
	Buckets:   []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 120, 300, 600, 900},
}, []string{"type", "outcome"})

// ObserveJob records one finished agent job. outcome is OutcomeSucceeded or
// OutcomeFailed. It is safe for concurrent use, so the orchestrator can call it
// from whichever goroutine finalized the job.
func ObserveJob(jobType, outcome string, d time.Duration) {
	jobDuration.WithLabelValues(jobType, outcome).Observe(d.Seconds())
}

// Handler serves the default Prometheus registry — this histogram plus the Go
// runtime and process collectors registered by default. Mount it at /metrics on
// a listener separate from any control or internet-facing port.
func Handler() http.Handler { return promhttp.Handler() }
