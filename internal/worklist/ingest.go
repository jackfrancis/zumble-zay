package worklist

import (
	"context"
	"log/slog"
)

// NoopIngestor records ingestion intent without performing work. It is the
// placeholder until the orchestrator and agent runtimes exist; the real
// serialized agentic flow will implement the same Ingestor interface.
type NoopIngestor struct {
	Log *slog.Logger
}

// EnsureBackfill logs the request and returns immediately. It is trivially
// idempotent (it does nothing).
func (n NoopIngestor) EnsureBackfill(_ context.Context, ownerID string) error {
	if n.Log != nil {
		n.Log.Info("ingestion requested", "owner", ownerID, "impl", "noop")
	}
	return nil
}
