package opensandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// apiKeyHeader authenticates every lifecycle call (OpenSandbox API-key auth).
const apiKeyHeader = "OPEN-SANDBOX-API-KEY"

// client is a minimal HTTP client for the OpenSandbox lifecycle API (create, get,
// delete a sandbox; resolve a port's endpoint) plus the per-sandbox execd command
// call that starts the runtime. baseURL is the lifecycle base including the
// version prefix (e.g. "https://opensandbox:8443/v1"); execd calls target a
// separately resolved address.
type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(baseURL, apiKey string, hc *http.Client) *client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &client{baseURL: baseURL, apiKey: apiKey, http: hc}
}

// imageSpec is the container image source for a sandbox.
type imageSpec struct {
	URI string `json:"uri"`
}

// createSandboxRequest is the POST /sandboxes body. resourceLimits is required in
// standard (image) mode; timeout (seconds) is the sandbox's self-reap TTL and is
// omitted when zero (no TTL). env is optional container env — this launcher
// injects the ZZ_* contract through the exec command instead (docs/adr/0027).
type createSandboxRequest struct {
	Image          *imageSpec        `json:"image,omitempty"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	Timeout        *int              `json:"timeout,omitempty"`
	ResourceLimits map[string]string `json:"resourceLimits,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// sandboxStatus is the lifecycle state reported by the server.
type sandboxStatus struct {
	State string `json:"state"`
}

// sandboxInfo is the subset of the sandbox resource this launcher reads.
type sandboxInfo struct {
	ID     string        `json:"id"`
	Status sandboxStatus `json:"status"`
}

// createSandbox provisions a sandbox (POST /sandboxes) and returns its id/state.
func (c *client) createSandbox(ctx context.Context, req createSandboxRequest) (*sandboxInfo, error) {
	var info sandboxInfo
	if err := c.do(ctx, http.MethodPost, "/sandboxes", req, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// getSandbox fetches a sandbox's current state (GET /sandboxes/{id}), used to
// poll for Running before the runtime is exec'd in.
func (c *client) getSandbox(ctx context.Context, id string) (*sandboxInfo, error) {
	var info sandboxInfo
	if err := c.do(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// deleteSandbox schedules a sandbox for termination (DELETE /sandboxes/{id}).
func (c *client) deleteSandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
}

// endpointInfo is the resolved network address (and any auth headers) for a
// service port inside a sandbox.
type endpointInfo struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// resolveEndpoint resolves the reachable address of a port inside the sandbox
// (GET /sandboxes/{id}/endpoints/{port}). With useServerProxy the server returns
// a URL routed through itself; otherwise it returns the workload's direct address
// (reachable in-cluster).
func (c *client) resolveEndpoint(ctx context.Context, id string, port int, useServerProxy bool) (*endpointInfo, error) {
	path := fmt.Sprintf("/sandboxes/%s/endpoints/%d?use_server_proxy=%t", url.PathEscape(id), port, useServerProxy)
	var ep endpointInfo
	if err := c.do(ctx, http.MethodGet, path, nil, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// runCommandRequest is the execd POST /command body. Background runs the command
// detached, so the call returns once execd has accepted it while the process
// keeps running.
type runCommandRequest struct {
	Command    string            `json:"command"`
	Background bool              `json:"background"`
	Envs       map[string]string `json:"envs,omitempty"`
}

// execCommand starts a command inside a sandbox through its execd endpoint
// (POST {execdBaseURL}/command). It targets the resolved execd address rather than
// the lifecycle server, forwards the endpoint's auth headers (e.g. the execd
// access token) plus the API key (for the server-proxy path), and returns as soon
// as execd accepts the background command — the detached process continues, so the
// streamed response is not consumed.
func (c *client) execCommand(ctx context.Context, execdBaseURL string, headers map[string]string, body runCommandRequest) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opensandbox: marshal command: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, execdBaseURL+"/command", bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("opensandbox: build command request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, c.apiKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opensandbox: exec command: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("opensandbox: exec command: status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}

// do performs one lifecycle request: it marshals body (when non-nil), sets the
// API key and content type, and decodes a 2xx JSON response into out (when
// non-nil). A non-2xx status is returned as an error carrying a bounded snippet
// of the response body for diagnosis.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opensandbox: marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("opensandbox: build request: %w", err)
	}
	req.Header.Set(apiKeyHeader, c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if out != nil {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opensandbox: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("opensandbox: %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(snippet))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("opensandbox: decode response: %w", err)
		}
	}
	return nil
}
