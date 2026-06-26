// ZZClient is the agent runtime's view of ZZ.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// ZZClient is the substrate-neutral HTTP contract every agent runtime speaks
// back to ZZ, regardless of where it runs — in-process today; a kagent service,
// an agent-sandbox Pod, or a custom workload tomorrow. It is the portability
// boundary defined in docs/adr/0009: a runtime is anything that holds a
// ZZ-minted job token and makes these two calls:
//
//  1. VendCredential — exchange the job token for the acting user's delegated
//     provider credential (ZZ is a credential broker, not a data broker; see
//     docs/adr/0006). The runtime then calls the provider directly.
//  2. Ingest — post the normalized WorkItems back to ZZ's sink.
//
// Both calls authenticate with the job token as a bearer credential over the
// Authorization header only (never a query string), per the security
// invariants. The client deliberately knows nothing about any provider, so a
// runtime on any substrate reimplements only these two calls.
type ZZClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewZZClient builds a client for ZZ's agent-plane contract at baseURL,
// authenticating with the supplied ZZ-minted job token. A nil httpClient gets a
// default with a sane timeout.
func NewZZClient(baseURL, token string, httpClient *http.Client) *ZZClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &ZZClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: httpClient}
}

// Credential is the delegated provider credential ZZ vends to a runtime. The
// refresh token is intentionally withheld by ZZ; a runtime only needs the
// access token to call the provider directly.
type Credential struct {
	Provider    string `json:"provider"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Expiry      string `json:"expiry"`
}

// VendCredential performs the first call of the contract:
// POST /agent/credentials/{provider}, returning the acting user's credential
// for the named provider.
func (c *ZZClient) VendCredential(ctx context.Context, provider string) (Credential, error) {
	u := c.baseURL + "/agent/credentials/" + provider
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return Credential{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Credential{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var cred Credential
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cred); err != nil {
		return Credential{}, err
	}
	if cred.AccessToken == "" {
		return Credential{}, fmt.Errorf("empty access token")
	}
	return cred, nil
}

// Ingest performs the second call of the contract: POST /agent/worklist, handing
// ZZ the normalized work items the runtime produced. ZZ stamps provenance and
// persists them.
func (c *ZZClient) Ingest(ctx context.Context, items []worklist.WorkItem) error {
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return err
	}
	u := c.baseURL + "/agent/worklist"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// ListWorklist performs the read side of the contract: GET /agent/worklist,
// returning the acting user's persisted work items so a runtime can augment
// them in place rather than re-deriving them from the provider (docs/adr/0010).
// A positive limit requests only the top-N items by rank, bounding expensive
// per-item enrichment.
func (c *ZZClient) ListWorklist(ctx context.Context, limit int) ([]worklist.WorkItem, error) {
	u := c.baseURL + "/agent/worklist"
	if limit > 0 {
		u += "?limit=" + strconv.Itoa(limit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Items []worklist.WorkItem `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Items, nil
}
