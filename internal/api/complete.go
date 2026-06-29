package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackfrancis/zumble-zay/internal/principal"
)

// CompletionReporter forwards a runtime's terminal completion to the control
// plane (docs/adr/0024). controlplane.Client satisfies it; depending only on
// this seam keeps the api package off the control plane's concrete type, as the
// worklist and conversation handlers do for their enqueuers.
type CompletionReporter interface {
	Complete(ctx context.Context, jobID, errMsg string) error
}

// CompleteHandler receives a runtime's terminal completion report (POST
// /agent/complete) and forwards it to the orchestrator, so a job is finalized
// the instant the runtime finishes rather than when the substrate watch observes
// it (docs/adr/0024). It is the optional third call of the runtime contract
// (docs/adr/0009): vend, ingest, then report completion.
type CompleteHandler struct {
	reporter CompletionReporter
	log      *slog.Logger
}

// NewCompleteHandler constructs a CompleteHandler that forwards via reporter.
func NewCompleteHandler(reporter CompletionReporter, log *slog.Logger) *CompleteHandler {
	return &CompleteHandler{reporter: reporter, log: log}
}

type completeRequest struct {
	Error string `json:"error,omitempty"`
}

// Complete handles POST /agent/complete. The job is identified by the workload
// token's job id (carried on the principal), so a runtime can only complete its
// own job — never another's. Forwarding is best-effort: a failure is logged and
// the orchestrator's substrate watch still backstops completion, so the runtime
// always receives 202.
func (h *CompleteHandler) Complete(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok || p.JobID == "" {
		writeError(w, http.StatusBadRequest, "no job in scope")
		return
	}
	var req completeRequest
	// An empty body is allowed: success with no detail.
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	if err := h.reporter.Complete(r.Context(), p.JobID, req.Error); err != nil && h.log != nil {
		h.log.Warn("forward runtime completion failed", "job", p.JobID, "err", err)
	}
	w.WriteHeader(http.StatusAccepted)
}
