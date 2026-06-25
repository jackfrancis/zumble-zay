// Package github is the GitHub provider client. It is imported only by agent
// runtimes — never by ZZ core packages — because ZZ is a credential broker, not
// a data broker: the agent connects to GitHub directly (see docs/adr/0006).
//
// It retrieves a user's pull requests via the search API using the user's own
// vended token, so `@me` resolves to that user and public results need no extra
// scope. Results map to worklist.WorkItem with default ZZ metadata; analysis
// agents decorate priority/relevance/impact later.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

const (
	defaultBaseURL = "https://api.github.com"
	perPage        = 50
	maxBody        = 8 << 20 // 8 MiB
)

// Client retrieves work signals from GitHub.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient returns a GitHub client. A nil httpClient uses http.DefaultClient;
// an empty baseURL uses the public API (tests point it at a stub).
func NewClient(httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{http: httpClient, baseURL: strings.TrimRight(baseURL, "/")}
}

// signals are the (reason, query) pairs retrieved for the authenticated user.
// The reason records why an item surfaced and feeds the Relevance/Urgency axes;
// archived:false keeps stale archived-repo items out of the worklist.
var signals = []struct {
	reason worklist.Reason
	query  string
}{
	{worklist.ReasonAuthor, "is:pr is:open author:@me archived:false"},
	{worklist.ReasonAssignee, "is:pr is:open assignee:@me archived:false"},
	{worklist.ReasonReviewRequested, "is:pr is:open review-requested:@me archived:false"},
}

// FetchWorklist retrieves the user's authored, assigned, and review-requested
// pull requests and returns them deduplicated by item ID. When an item surfaces
// under more than one query, the reasons are merged onto a single work item.
func (c *Client) FetchWorklist(ctx context.Context, token string) ([]worklist.WorkItem, error) {
	now := time.Now().UTC()
	seen := make(map[string]worklist.WorkItem)
	for _, s := range signals {
		items, err := c.searchIssues(ctx, token, s.query)
		if err != nil {
			return nil, fmt.Errorf("github %s search: %w", s.reason, err)
		}
		for _, it := range items {
			wi, ok := it.toWorkItem(s.reason, now)
			if !ok {
				continue
			}
			if existing, dup := seen[wi.ID]; dup {
				existing.Signals.Reasons = appendReason(existing.Signals.Reasons, s.reason)
				seen[wi.ID] = existing
				continue
			}
			seen[wi.ID] = wi
		}
	}
	out := make([]worklist.WorkItem, 0, len(seen))
	for _, wi := range seen {
		out = append(out, wi)
	}
	return out, nil
}

// appendReason adds r to rs if not already present, keeping reasons unique and
// in first-seen order.
func appendReason(rs []worklist.Reason, r worklist.Reason) []worklist.Reason {
	for _, x := range rs {
		if x == r {
			return rs
		}
	}
	return append(rs, r)
}

type searchResponse struct {
	Items []searchItem `json:"items"`
}

type searchItem struct {
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	HTMLURL       string    `json:"html_url"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Comments      int       `json:"comments"`
	RepositoryURL string    `json:"repository_url"`
	Labels        []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Milestone *struct {
		DueOn time.Time `json:"due_on"`
	} `json:"milestone"`
	Reactions *struct {
		TotalCount int `json:"total_count"`
	} `json:"reactions"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

func (it searchItem) toWorkItem(reason worklist.Reason, now time.Time) (worklist.WorkItem, bool) {
	// Defensive: the search/issues endpoint can return issues; keep only PRs.
	if it.PullRequest == nil {
		return worklist.WorkItem{}, false
	}
	repo := strings.TrimPrefix(it.RepositoryURL, defaultBaseURL+"/repos/")

	// Signals carried directly by the search response — no extra API calls.
	sig := worklist.Signals{
		Reasons:        []worklist.Reason{reason},
		Comments:       it.Comments,
		OpenedAt:       it.CreatedAt,
		LastActivityAt: it.UpdatedAt,
		ObservedAt:     now,
	}
	if it.Reactions != nil {
		sig.Reactions = it.Reactions.TotalCount
	}
	if it.Milestone != nil {
		sig.DeadlineAt = it.Milestone.DueOn
	}
	for _, l := range it.Labels {
		sig.Labels = append(sig.Labels, l.Name)
	}

	return worklist.WorkItem{
		ID:     "github:" + repo + "#" + strconv.Itoa(it.Number),
		Source: "github",
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{
			Number:    it.Number,
			Repo:      repo,
			Title:     it.Title,
			URL:       it.HTMLURL,
			State:     it.State,
			UpdatedAt: it.UpdatedAt,
		},
		Signals: sig,
		Meta:    worklist.Metadata{Origin: worklist.OriginAgent},
	}, true
}

func (c *Client) searchIssues(ctx context.Context, token, q string) ([]searchItem, error) {
	u := c.baseURL + "/search/issues?per_page=" + strconv.Itoa(perPage) + "&q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "zumble-zay-agent")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, err
	}
	return sr.Items, nil
}

// get performs an authenticated GET and returns the response body.
func (c *Client) get(ctx context.Context, token, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "zumble-zay-agent")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}

// Login returns the authenticated user's GitHub login, needed to attribute
// review-request timeline events to "me".
func (c *Client) Login(ctx context.Context, token string) (string, error) {
	body, err := c.get(ctx, token, "/user")
	if err != nil {
		return "", err
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", fmt.Errorf("github: empty login")
	}
	return u.Login, nil
}

type timelineEvent struct {
	Event             string    `json:"event"`
	CreatedAt         time.Time `json:"created_at"`
	SubmittedAt       time.Time `json:"submitted_at"`
	RequestedReviewer *struct {
		Login string `json:"login"`
	} `json:"requested_reviewer"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
}

// AwaitingMeSince reports when login was last asked to review the PR with no
// review submitted by login since — i.e. how long it has been blocked on them.
// A zero time means nothing is outstanding. It reads a single page of the issue
// timeline; that is sufficient for recent review requests.
func (c *Client) AwaitingMeSince(ctx context.Context, token, repo string, number int, login string) (time.Time, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/timeline?per_page=%d", repo, number, perPage)
	body, err := c.get(ctx, token, path)
	if err != nil {
		return time.Time{}, err
	}
	var events []timelineEvent
	if err := json.Unmarshal(body, &events); err != nil {
		return time.Time{}, err
	}
	var requestedAt, reviewedAt time.Time
	for _, e := range events {
		switch e.Event {
		case "review_requested":
			if e.RequestedReviewer != nil && e.RequestedReviewer.Login == login && e.CreatedAt.After(requestedAt) {
				requestedAt = e.CreatedAt
			}
		case "reviewed":
			if e.User != nil && e.User.Login == login {
				at := e.SubmittedAt
				if at.IsZero() {
					at = e.CreatedAt
				}
				if at.After(reviewedAt) {
					reviewedAt = at
				}
			}
		}
	}
	if requestedAt.IsZero() || reviewedAt.After(requestedAt) {
		return time.Time{}, nil
	}
	return requestedAt, nil
}
