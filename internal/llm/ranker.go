// Package llm is the AxisRanker backed by a chat model. It is imported only by
// agent runtimes — never by ZZ core — because ZZ is a credential broker, not a
// model broker: the ranking model is called from the runtime, behind the
// worklist.AxisRanker seam (docs/adr/0006, 0011).
//
// It speaks the OpenAI-compatible chat-completions API, so any endpoint exposing
// POST /chat/completions works (GitHub Copilot, OpenAI, Azure OpenAI, a
// self-hosted gateway); the provider is a config value, not a code change. The
// default targets GitHub Copilot.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

const (
	// DefaultEndpoint targets GitHub Copilot's chat-completions API.
	DefaultEndpoint = "https://api.githubcopilot.com/chat/completions"
	// DefaultModel is the default ranking model; override with config/AI_MODEL.
	DefaultModel = "claude-opus-4.8"
	// copilotIntegrationID is required by Copilot's endpoint and sent only when
	// the endpoint host is a Copilot host.
	copilotIntegrationID = "copilot-developer-cli"
	maxBody              = 1 << 20 // 1 MiB; ranking responses are tiny
)

// Config configures the chat-model ranker.
type Config struct {
	Endpoint string       // chat-completions URL; empty uses DefaultEndpoint
	Model    string       // model identifier; empty uses DefaultModel
	Token    string       // bearer token (e.g. a Copilot copilot_chat PAT)
	Client   *http.Client // shared HTTP client; nil gets a default
	Logger   *slog.Logger // diagnostics (converse tool loop); nil uses slog.Default()
}

// Ranker implements worklist.AxisRanker by asking a chat model to score one
// item's four axes. ZZ ratifies the proposal against the deterministic baseline
// (confidence gate + deviation clamp), so a misbehaving model cannot hijack
// ordering (docs/adr/0011).
type Ranker struct {
	endpoint string
	model    string
	token    string
	client   *http.Client
}

var _ worklist.AxisRanker = (*Ranker)(nil)

// NewRanker builds a Ranker, applying defaults for the endpoint and model.
func NewRanker(cfg Config) *Ranker {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Ranker{endpoint: cfg.Endpoint, model: cfg.Model, token: cfg.Token, client: cfg.Client}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is set on an assistant message that requests tool invocations;
	// ToolCallID links a role=="tool" result back to the call it answers
	// (docs/adr/0020).
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// FinishReason is response-only diagnostic context ("stop", "tool_calls",
	// ...); it is never serialized into a request.
	FinishReason string `json:"-"`
	// raw is the unparsed response body, retained for diagnostics when a tool-call
	// turn does not parse as expected (unexported -> never serialized).
	raw []byte
}

// chatToolCall is one tool invocation the model requested.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded arguments
	} `json:"function"`
}

// chatTool advertises a callable function to the model.
type chatTool struct {
	Type     string           `json:"type"` // "function"
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Tools          []chatTool      `json:"tools,omitempty"`
	ToolChoice     string          `json:"tool_choice,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
			// FunctionCall is the legacy single-function shape some OpenAI-compatible
			// gateways still return instead of tool_calls; parsed as a fallback.
			FunctionCall *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// axesDoc is the strict JSON the model is asked to return.
type axesDoc struct {
	Relevance  float64 `json:"relevance"`
	Impact     float64 `json:"impact"`
	Engagement float64 `json:"engagement"`
	Urgency    float64 `json:"urgency"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

// Propose asks the model to score the item and maps the response onto an
// AxisProposal. The values are clamped to [0,1] defensively; ZZ ratifies them
// against the baseline regardless.
func (r *Ranker) Propose(ctx context.Context, item worklist.WorkItem) (worklist.AxisProposal, error) {
	content, err := chatComplete(ctx, r.client, r.endpoint, r.token, chatRequest{
		Model: r.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt(item)},
		},
		Temperature:    0,
		ResponseFormat: &responseFormat{Type: "json_object"},
	})
	if err != nil {
		return worklist.AxisProposal{}, err
	}

	var doc axesDoc
	if err := json.Unmarshal([]byte(stripFences(content)), &doc); err != nil {
		return worklist.AxisProposal{}, fmt.Errorf("parse axes JSON: %w", err)
	}
	return worklist.AxisProposal{
		Relevance:  clamp01(doc.Relevance),
		Impact:     clamp01(doc.Impact),
		Engagement: clamp01(doc.Engagement),
		Urgency:    clamp01(doc.Urgency),
		Confidence: clamp01(doc.Confidence),
		Rationale:  strings.TrimSpace(doc.Rationale),
	}, nil
}

// chatComplete posts an OpenAI-compatible chat-completions request and returns
// the assistant message content. Shared by the ranker and the converser; it
// requires non-empty content, so it is for the no-tools case.
func chatComplete(ctx context.Context, httpClient *http.Client, endpoint, token string, body chatRequest) (string, error) {
	msg, err := chat(ctx, httpClient, endpoint, token, body)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(msg.Content) == "" {
		return "", fmt.Errorf("chat returned no content")
	}
	return msg.Content, nil
}

// chat posts a chat-completions request and returns the assistant's message,
// including any tool calls. Unlike chatComplete it does not require content,
// since a tool-calling turn returns tool_calls with empty content (docs/adr/0020).
// It sends the Copilot integration header when the endpoint is a Copilot host.
func chat(ctx context.Context, httpClient *http.Client, endpoint, token string, body chatRequest) (chatMessage, error) {
	reqBody, err := json.Marshal(body)
	if err != nil {
		return chatMessage{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return chatMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if isCopilotHost(endpoint) {
		req.Header.Set("Copilot-Integration-Id", copilotIntegrationID)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return chatMessage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return chatMessage{}, fmt.Errorf("chat status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return chatMessage{}, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("chat returned no choices")
	}
	// Aggregate across choices. The OpenAI standard returns a single choice, but
	// some gateways (Copilot serving Claude) split one assistant turn into
	// several choices — a text block in one, each tool call in its own — so the
	// tool calls live beyond choices[0]. Collect content and tool calls from all
	// choices so a multi-choice tool turn parses the same as a single-choice one.
	var (
		content   strings.Builder
		toolCalls []chatToolCall
	)
	for i := range cr.Choices {
		cm := cr.Choices[i].Message
		if s := strings.TrimSpace(cm.Content); s != "" {
			if content.Len() > 0 {
				content.WriteString("\n")
			}
			content.WriteString(cm.Content)
		}
		toolCalls = append(toolCalls, cm.ToolCalls...)
		// Fallback for the legacy single-function shape (function_call) some
		// gateways return instead of tool_calls.
		if cm.FunctionCall != nil && cm.FunctionCall.Name != "" {
			tc := chatToolCall{ID: fmt.Sprintf("call_%d", i), Type: "function"}
			tc.Function.Name = cm.FunctionCall.Name
			tc.Function.Arguments = cm.FunctionCall.Arguments
			toolCalls = append(toolCalls, tc)
		}
	}
	return chatMessage{
		Role:         "assistant",
		Content:      content.String(),
		ToolCalls:    toolCalls,
		FinishReason: cr.Choices[0].FinishReason,
		raw:          raw,
	}, nil
}

// isCopilotHost reports whether the endpoint's host is a GitHub Copilot host, in
// which case the Copilot-Integration-Id header is required.
func isCopilotHost(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "githubcopilot.com" || strings.HasSuffix(h, ".githubcopilot.com")
}

// stripFences removes a ```json ... ``` markdown fence if the model wrapped its
// JSON in one despite the response_format request.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}
