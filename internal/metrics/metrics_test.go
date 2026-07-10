package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestObserveJobIsExposed(t *testing.T) {
	ObserveJob("github-converse", OutcomeSucceeded, 3*time.Second)
	ObserveJob("github-converse", OutcomeFailed, 12*time.Second)

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The histogram's _count series carries the labels; the exposition format
	// sorts labels alphabetically (outcome before type).
	for _, want := range []string{
		`zz_agent_job_duration_seconds_count{outcome="succeeded",type="github-converse"}`,
		`zz_agent_job_duration_seconds_count{outcome="failed",type="github-converse"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metric %q not exposed; body:\n%s", want, body)
		}
	}
}
