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
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
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
	reqBody, err := json.Marshal(chatRequest{
		Model: r.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt(item)},
		},
		Temperature:    0,
		ResponseFormat: &responseFormat{Type: "json_object"},
	})
	if err != nil {
		return worklist.AxisProposal{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return worklist.AxisProposal{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	if isCopilotHost(r.endpoint) {
		req.Header.Set("Copilot-Integration-Id", copilotIntegrationID)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return worklist.AxisProposal{}, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return worklist.AxisProposal{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return worklist.AxisProposal{}, fmt.Errorf("chat status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return worklist.AxisProposal{}, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return worklist.AxisProposal{}, fmt.Errorf("chat returned no content")
	}

	var doc axesDoc
	if err := json.Unmarshal([]byte(stripFences(cr.Choices[0].Message.Content)), &doc); err != nil {
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
