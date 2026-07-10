package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestObserveJobIsExposed(t *testing.T) {
	ObserveJob("github-converse", "substrate", OutcomeSucceeded, 3*time.Second)
	ObserveJob("github-converse", "substrate", OutcomeFailed, 12*time.Second)
	ObserveQueueWait("github-converse", 200*time.Millisecond)
	ObserveDispatch("substrate", "github-converse", 900*time.Millisecond)
	ObserveProvisioning("substrate", "github-converse", 1.2)
	ObserveRuntimeWork("substrate", "github-converse", 25)
	ObserveModelSeconds("github-converse", 7)
	ObserveModelCalls("github-converse", 4)
	ObserveToolCalls("github-converse", 3)

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The histogram's _count series carries the labels; the exposition format
	// sorts labels alphabetically (launcher, outcome, type).
	for _, want := range []string{
		`zz_agent_job_duration_seconds_count{launcher="substrate",outcome="succeeded",type="github-converse"}`,
		`zz_agent_job_duration_seconds_count{launcher="substrate",outcome="failed",type="github-converse"}`,
		`zz_agent_job_queue_wait_seconds_count{type="github-converse"}`,
		`zz_agent_job_dispatch_duration_seconds_count{launcher="substrate",type="github-converse"}`,
		`zz_agent_job_provisioning_seconds_count{launcher="substrate",type="github-converse"}`,
		`zz_agent_job_runtime_seconds_count{launcher="substrate",type="github-converse"}`,
		`zz_agent_job_model_seconds_count{type="github-converse"}`,
		`zz_agent_job_model_calls_count{type="github-converse"}`,
		`zz_agent_job_tool_calls_count{type="github-converse"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metric %q not exposed; body:\n%s", want, body)
		}
	}
}
