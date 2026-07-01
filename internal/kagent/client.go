package kagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// client is a minimal JSON-RPC client for a kagent controller's A2A endpoint. It
// dispatches one agent task (message/send) to a named durable Agent and reads the
// terminal task back. baseURL is the controller root (e.g.
// "http://kagent-controller.kagent.svc.cluster.local:8083"); the per-agent path
// is /api/a2a/{namespace}/{name}/.
type client struct {
	baseURL string
	http    *http.Client
}

func newClient(baseURL string, hc *http.Client) *client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &client{baseURL: baseURL, http: hc}
}

// rpcRequest is a JSON-RPC 2.0 A2A message/send call.
type rpcRequest struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      string     `json:"id"`
	Method  string     `json:"method"`
	Params  sendParams `json:"params"`
}

type sendParams struct {
	Message       a2aMessage `json:"message"`
	Configuration sendConfig `json:"configuration"`
}

// sendConfig sets A2A message-send options. Blocking is false so the controller
// acknowledges as soon as the agent accepts the task, instead of holding the
// connection until the job finishes: the agent runs the job detached and reports
// completion out-of-band (docs/adr/0025). This is what keeps a long job from
// tripping the controller's synchronous-proxy timeout.
type sendConfig struct {
	Blocking bool `json:"blocking"`
}

type a2aMessage struct {
	Role      string            `json:"role"`
	Kind      string            `json:"kind"`
	MessageID string            `json:"messageId"`
	Parts     []textPart        `json:"parts"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type textPart struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// taskResult is the subset of a terminal A2A task this launcher acts on.
type taskResult struct {
	TaskID  string
	State   string
	Message string
}

// sendTask posts a blocking message/send to the named agent and returns its
// terminal task. The call is bounded by ctx (the orchestrator's per-job
// deadline), so the client sets no timeout of its own.
func (c *client) sendTask(ctx context.Context, namespace, agentName, prompt string, metadata map[string]string) (taskResult, error) {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      randID(),
		Method:  "message/send",
		Params: sendParams{
			Message: a2aMessage{
				Role: "user", Kind: "message", MessageID: randID(),
				Parts:    []textPart{{Kind: "text", Text: prompt}},
				Metadata: metadata,
			},
			Configuration: sendConfig{Blocking: false},
		},
	})
	if err != nil {
		return taskResult{}, err
	}

	u := fmt.Sprintf("%s/api/a2a/%s/%s/", c.baseURL, url.PathEscape(namespace), url.PathEscape(agentName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return taskResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return taskResult{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return taskResult{}, fmt.Errorf("status %d: %s", resp.StatusCode, snippet(data))
	}

	var rpcResp struct {
		Result *struct {
			ID     string `json:"id"`
			Status struct {
				State   string `json:"state"`
				Message struct {
					Parts []textPart `json:"parts"`
				} `json:"message"`
			} `json:"status"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return taskResult{}, fmt.Errorf("decode response: %w", err)
	}
	if rpcResp.Error != nil {
		return taskResult{}, fmt.Errorf("a2a error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil {
		return taskResult{}, fmt.Errorf("response had neither result nor error")
	}
	res := taskResult{TaskID: rpcResp.Result.ID, State: rpcResp.Result.Status.State}
	if len(rpcResp.Result.Status.Message.Parts) > 0 {
		res.Message = rpcResp.Result.Status.Message.Parts[0].Text
	}
	return res, nil
}

func snippet(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

func randID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "id"
	}
	return hex.EncodeToString(b[:])
}
