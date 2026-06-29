package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/principal"
)

type fakeReporter struct {
	called bool
	jobID  string
	errMsg string
}

func (f *fakeReporter) Complete(_ context.Context, jobID, errMsg string) error {
	f.called = true
	f.jobID, f.errMsg = jobID, errMsg
	return nil
}

func TestCompleteForwardsJobFromPrincipal(t *testing.T) {
	rep := &fakeReporter{}
	h := NewCompleteHandler(rep, nil)

	req := httptest.NewRequest(http.MethodPost, "/agent/complete", strings.NewReader(`{"error":"boom"}`))
	req = req.WithContext(principal.NewContext(req.Context(), &principal.Principal{
		Kind:    principal.KindWorkload,
		Subject: "runtime-job-1",
		JobID:   "job-1",
		Scopes:  []principal.Scope{principal.ScopeSignalsRead},
	}))
	w := httptest.NewRecorder()
	h.Complete(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if !rep.called || rep.jobID != "job-1" || rep.errMsg != "boom" {
		t.Fatalf("not forwarded correctly: called=%v job=%q err=%q", rep.called, rep.jobID, rep.errMsg)
	}
}

func TestCompleteRejectsMissingJob(t *testing.T) {
	rep := &fakeReporter{}
	h := NewCompleteHandler(rep, nil)

	// No principal in context: a runtime can only complete its own job, named by
	// the token's job id, so an absent one is rejected and never forwarded.
	req := httptest.NewRequest(http.MethodPost, "/agent/complete", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.Complete(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if rep.called {
		t.Fatal("reporter must not be called without a job in scope")
	}
}
