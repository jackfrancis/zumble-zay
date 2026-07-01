// Package agenta2a serves the ZZ agent runtime (internal/agent) over the A2A
// protocol, so a durable A2A host such as kagent can dispatch ZZ jobs to a
// long-running runtime instead of spawning an ephemeral pod per job
// (docs/adr/0024). It is a new, additive way to invoke the exact same
// agent.Run; the pod launchers and cmd/runtime are untouched.
//
// Each A2A message/send task carries the per-job parameters (job type, ZZ job
// token, provider, item) in its message metadata — the channel a kagent
// controller forwards intact, unlike HTTP headers — while static configuration
// (ZZ base URL, model endpoint/token) comes from the Deployment environment.
// The two are merged through the same agent.ParamsFromEnv contract the pod
// launchers fill via env, so a runtime behaves identically on either substrate
// and the encode/decode halves cannot drift.
package agenta2a

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackfrancis/zumble-zay/internal/agent"
)

// Server adapts the agent runtime to the A2A protocol. It is safe for concurrent
// use: each request builds its own RunParams and runs independently.
type Server struct {
	getenv func(string) string
	run    func(context.Context, agent.RunParams) error
	log    *slog.Logger
}

// Option configures a Server.
type Option func(*Server)

// WithGetenv overrides the static-configuration source (default os.Getenv).
func WithGetenv(f func(string) string) Option { return func(s *Server) { s.getenv = f } }

// WithRun overrides the job runner (default agent.Run); tests inject a fake to
// assert the metadata→params mapping without a real ZZ backend.
func WithRun(f func(context.Context, agent.RunParams) error) Option {
	return func(s *Server) { s.run = f }
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// New builds a Server. By default it reads static config from the process
// environment and executes jobs with agent.Run.
func New(opts ...Option) *Server {
	s := &Server{getenv: os.Getenv, run: agent.Run, log: slog.Default()}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.run == nil {
		s.run = agent.Run
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Handler returns the A2A HTTP handler: the agent card for discovery/readiness
// and the JSON-RPC endpoint for message/send.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", s.handleCard)
	mux.HandleFunc("/", s.handleRPC)
	return mux
}

// handleCard serves the A2A agent card. kagent's readiness probe hits this path,
// so it must succeed without any per-job state.
func (s *Server) handleCard(w http.ResponseWriter, _ *http.Request) {
	skill := func(id, name, desc string) map[string]any {
		return map[string]any{"id": id, "name": name, "description": desc, "tags": []string{"zumble-zay"}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":               "zumble_zay_runtime",
		"description":        "Runs Zumble-Zay agent jobs dispatched over A2A; job type and job token ride in message metadata.",
		"version":            "0.1.0",
		"protocolVersion":    "0.3",
		"preferredTransport": "JSONRPC",
		"defaultInputModes":  []string{"text"},
		"defaultOutputModes": []string{"text"},
		"capabilities":       map[string]any{"streaming": false},
		"skills": []map[string]any{
			skill(agent.JobIngest, "Ingest", "Fetch the user's GitHub work into ZZ."),
			skill(agent.JobEnrich, "Enrich", "Augment stored items with per-item signals."),
			skill(agent.JobRank, "Rank", "Produce the four ranking axes for the worklist."),
			skill(agent.JobConverse, "Converse", "Answer one assistive conversation turn for an item."),
			skill(agent.JobResearch, "Research", "Re-weight an item's axes from its thread."),
		},
	})
}

// taskMessage is the subset of the A2A message we read: identifiers for the task
// envelope and the metadata that carries the per-job parameters.
type taskMessage struct {
	MessageID string         `json:"messageId"`
	ContextID string         `json:"contextId"`
	TaskID    string         `json:"taskId"`
	Metadata  map[string]any `json:"metadata"`
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Message taskMessage `json:"message"`
	} `json:"params"`
}

// handleRPC accepts one A2A message/send, starts the ZZ job detached, and
// acknowledges it immediately with a non-terminal task. The job does NOT run
// synchronously: the kagent controller caps a synchronous message/send (~180s),
// so a long job — a substantive converse review — must not depend on holding this
// connection open. Instead the job runs on its own context and reports its
// terminal outcome to ZZ through the runtime completion callback (docs/adr/0025),
// which the orchestrator races against the launcher's per-job deadline
// (docs/adr/0024). The only failure surfaced through this response is a malformed
// request rejected before the job starts.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	if req.Method != "message/send" {
		writeRPCError(w, req.ID, -32601, "method not supported: "+req.Method)
		return
	}

	p, err := s.paramsFromTask(req.Params.Message.Metadata)
	if err != nil {
		// Never log the metadata itself — it carries the job token.
		s.log.Warn("a2a task rejected: invalid job parameters", "err", err)
		writeTask(w, req.ID, req.Params.Message, "failed", "invalid job parameters: "+err.Error())
		return
	}

	// Report terminal completion to ZZ (docs/adr/0025): the orchestrator finalizes
	// the job the instant this lands, racing it against the launcher's deadline.
	p.ReportCompletion = true

	// Run detached from the request: its own context bounded by the job budget,
	// because r.Context() is canceled the moment this response returns. This
	// decouples the job's runtime from the controller's synchronous-proxy timeout.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), agent.JobTimeout(p.JobType))
		defer cancel()
		if runErr := s.run(ctx, p); runErr != nil {
			s.log.Error("a2a job failed", "job_type", p.JobType, "err", runErr)
			return
		}
		s.log.Info("a2a job completed", "job_type", p.JobType)
	}()

	s.log.Info("a2a job accepted", "job_type", p.JobType, "provider", p.Provider, "item", p.ItemID)
	writeTask(w, req.ID, req.Params.Message, "submitted", "job "+p.JobType+" accepted")
}

// paramsFromTask reconstructs RunParams by reading each ZZ_* key from the task
// metadata first and falling back to the process environment. This reuses the
// launchers' agent.ParamsFromEnv verbatim, so the injection contract has exactly
// one decoder: per-job values (job type, token, provider, item) come from
// metadata; static config (base URL, model endpoint/token) from the Deployment
// environment; and the model token, never emitted into metadata, stays env-only.
func (s *Server) paramsFromTask(metadata map[string]any) (agent.RunParams, error) {
	lookup := func(key string) string {
		if v, ok := metadata[key]; ok {
			if str, ok := v.(string); ok {
				return str
			}
			return fmt.Sprint(v)
		}
		return s.getenv(key)
	}
	return agent.ParamsFromEnv(lookup)
}

// writeTask writes a JSON-RPC result carrying an A2A task in a terminal state.
func writeTask(w http.ResponseWriter, id json.RawMessage, msg taskMessage, state, text string) {
	status := map[string]any{
		"state": state,
		"message": map[string]any{
			"kind": "message", "role": "agent", "messageId": randID(),
			"parts": []any{map[string]any{"kind": "text", "text": text}},
		},
	}
	result := map[string]any{
		"kind":      "task",
		"id":        firstNonEmpty(msg.TaskID, msg.MessageID, randID()),
		"contextId": firstNonEmpty(msg.ContextID, randID()),
		"status":    status,
	}
	if state == "completed" {
		result["artifacts"] = []any{map[string]any{
			"artifactId": randID(),
			"parts":      []any{map[string]any{"kind": "text", "text": text}},
		}}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"error":   map[string]any{"code": code, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func rawOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func randID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "id"
	}
	return hex.EncodeToString(b[:])
}
