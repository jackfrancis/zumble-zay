// Package agent is the ephemeral GitHub-ingestion runtime. It is the only
// component besides the composition root that imports a provider client: ZZ is
// a credential broker, not a data broker, so the agent connects to GitHub
// directly (see docs/adr/0006).
//
// A runtime carries a ZZ-minted job token and uses the same HTTP contract a
// future out-of-process Pod will use (docs/adr/0007): it vends the user's
// provider credential from ZZ, calls GitHub directly, and posts results back to
// ZZ's ingest sink. It never sees the user's raw token until ZZ vends it, and
// never writes anywhere but ZZ.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/github"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// RunParams configures a single runtime invocation.
type RunParams struct {
	BaseURL       string       // ZZ base URL (loopback in-process today)
	GitHubBaseURL string       // GitHub API base; empty uses the public API
	Client        *http.Client // shared HTTP client
	Token         string       // ZZ job token (bearer)
	Provider      string       // e.g. "github"
}

// Run executes the ingestion job: vend the provider credential from ZZ, fetch
// the user's work directly from GitHub, then post it to ZZ's ingest sink. An
// empty result is a successful no-op.
func Run(ctx context.Context, p RunParams) error {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 30 * time.Second}
	}
	cred, err := vendCredential(ctx, p)
	if err != nil {
		return fmt.Errorf("vend credential: %w", err)
	}
	items, err := github.NewClient(p.Client, p.GitHubBaseURL).FetchWorklist(ctx, cred.AccessToken)
	if err != nil {
		return fmt.Errorf("fetch github: %w", err)
	}
	if len(items) == 0 {
		return nil
	}
	if err := ingest(ctx, p, items); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}

type vendedCredential struct {
	Provider    string `json:"provider"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Expiry      string `json:"expiry"`
}

func vendCredential(ctx context.Context, p RunParams) (vendedCredential, error) {
	u := strings.TrimRight(p.BaseURL, "/") + "/agent/credentials/" + p.Provider
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return vendedCredential{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)

	resp, err := p.Client.Do(req)
	if err != nil {
		return vendedCredential{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return vendedCredential{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var c vendedCredential
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
		return vendedCredential{}, err
	}
	if c.AccessToken == "" {
		return vendedCredential{}, fmt.Errorf("empty access token")
	}
	return c, nil
}

func ingest(ctx context.Context, p RunParams, items []worklist.WorkItem) error {
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return err
	}
	u := strings.TrimRight(p.BaseURL, "/") + "/agent/worklist"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.Client.Do(req)
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
