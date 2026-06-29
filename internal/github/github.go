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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	State             string    `json:"state"` // for "reviewed": approved | changes_requested | commented | dismissed
	CreatedAt         time.Time `json:"created_at"`
	SubmittedAt       time.Time `json:"submitted_at"`
	RequestedReviewer *struct {
		Login string `json:"login"`
	} `json:"requested_reviewer"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	Actor *struct {
		Login string `json:"login"`
	} `json:"actor"`
}

// Activity holds the signals derived from a single read of an item's timeline.
type Activity struct {
	Participants        int       // distinct people who commented or reviewed
	InboundRefs         int       // cross-references from other issues/PRs (hub centrality)
	OtherReviewers      int       // distinct reviewers other than login (someone else is engaged)
	AwaitingMeSince     time.Time // when login was asked to review with no engagement since; zero if none
	AwaitingOthersSince time.Time // when the ball is in others' court (login had the last word, or a decisive review landed); zero if none
}

// ItemActivity reads one page of the issue/PR timeline and derives the
// engagement and centrality signals from it in a single call, plus how long the
// item has been blocked on login's review.
func (c *Client) ItemActivity(ctx context.Context, token, repo string, number int, login string) (Activity, error) {
	events, err := c.fetchTimeline(ctx, token, repo, number)
	if err != nil {
		return Activity{}, err
	}
	participants := make(map[string]struct{})
	otherReviewers := make(map[string]struct{})
	var inbound int
	// requestedAt: latest review explicitly requested of login. myLastActivityAt:
	// latest comment or review by login. lastActor/lastActivityAt: the most recent
	// voice on the thread, and lastActivityDecisive records whether that voice was
	// a formal review (changes requested / approved).
	var requestedAt, myLastActivityAt, lastActivityAt time.Time
	var lastActor string
	var lastActivityDecisive bool
	for _, e := range events {
		switch e.Event {
		case "commented":
			l := eventLogin(e)
			if l == "" {
				break
			}
			participants[l] = struct{}{}
			if e.CreatedAt.After(lastActivityAt) {
				lastActivityAt, lastActor, lastActivityDecisive = e.CreatedAt, l, false
			}
			if l == login && e.CreatedAt.After(myLastActivityAt) {
				myLastActivityAt = e.CreatedAt
			}
		case "reviewed":
			if e.User == nil || e.User.Login == "" {
				break
			}
			l := e.User.Login
			participants[l] = struct{}{}
			at := e.SubmittedAt
			if at.IsZero() {
				at = e.CreatedAt
			}
			if at.After(lastActivityAt) {
				lastActivityAt, lastActor = at, l
				lastActivityDecisive = e.State == "changes_requested" || e.State == "approved"
			}
			if l == login {
				if at.After(myLastActivityAt) {
					myLastActivityAt = at
				}
			} else {
				otherReviewers[l] = struct{}{}
			}
		case "cross-referenced":
			inbound++
		case "review_requested":
			if e.RequestedReviewer != nil && e.RequestedReviewer.Login == login && e.CreatedAt.After(requestedAt) {
				requestedAt = e.CreatedAt
			}
		}
	}
	a := Activity{Participants: len(participants), InboundRefs: inbound, OtherReviewers: len(otherReviewers)}

	// Decide whose court the ball is in from the most recent court-changing event.
	// Ball on login: a review was requested of them and they have not engaged
	// (commented or reviewed) since. Ball on others: login had the last word, or
	// someone's decisive review is the last word so progress is on the author —
	// even when login never formally reviewed (docs/adr/0015).
	var meSince, othersSince time.Time
	if !requestedAt.IsZero() && requestedAt.After(myLastActivityAt) {
		meSince = requestedAt
	}
	switch {
	case lastActor == login && !myLastActivityAt.IsZero():
		othersSince = myLastActivityAt
	case lastActivityDecisive:
		othersSince = lastActivityAt
	}
	switch {
	case meSince.After(othersSince):
		a.AwaitingMeSince = meSince
	case !othersSince.IsZero():
		a.AwaitingOthersSince = othersSince
	}
	return a, nil
}

func (c *Client) fetchTimeline(ctx context.Context, token, repo string, number int) ([]timelineEvent, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/timeline?per_page=%d", repo, number, perPage)
	body, err := c.get(ctx, token, path)
	if err != nil {
		return nil, err
	}
	var events []timelineEvent
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, err
	}
	return events, nil
}

func eventLogin(e timelineEvent) string {
	if e.Actor != nil && e.Actor.Login != "" {
		return e.Actor.Login
	}
	if e.User != nil && e.User.Login != "" {
		return e.User.Login
	}
	return ""
}

// Discussion is the live context for a single GitHub item, fetched for the
// assistive conversation (docs/adr/0019): its description, the most recent
// comments, and (for a PR) the changed file paths. Every field is bounded so the
// untrusted content stays capped and the prompt small.
type Discussion struct {
	Body         string
	Comments     []Comment
	ChangedFiles []string
}

// Comment is one comment in an item's discussion.
type Comment struct {
	Author string
	Body   string
	At     time.Time
}

const (
	maxDiscussionBody     = 4 << 10 // bytes of the description retained
	maxCommentBody        = 1 << 10 // bytes per comment retained
	maxDiscussionComments = 20      // most recent comments retained
	maxChangedFiles       = 50      // changed file paths retained
)

// Discussion fetches an item's description, its recent comments, and (for PRs)
// its changed file paths using the user's vended token. The body and comments
// are attacker-influenceable, so each is truncated and the counts capped; the
// caller frames the result as untrusted data (docs/adr/0019). Only the
// description is required: missing comments or files degrade gracefully. A repo
// the read-only token cannot see surfaces as an error the caller treats as "no
// extra context".
func (c *Client) Discussion(ctx context.Context, token, repo string, number int, isPR bool) (Discussion, error) {
	var d Discussion

	// Description from the issues endpoint (works for both issues and PRs).
	issueBody, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return Discussion{}, err
	}
	var issue struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(issueBody, &issue); err != nil {
		return Discussion{}, err
	}
	d.Body = truncateRunes(issue.Body, maxDiscussionBody)

	// Comments are best-effort: a missing discussion is not fatal.
	if body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number)); err == nil {
		var raw []struct {
			Body string `json:"body"`
			User *struct {
				Login string `json:"login"`
			} `json:"user"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(body, &raw); err == nil {
			if len(raw) > maxDiscussionComments {
				raw = raw[len(raw)-maxDiscussionComments:] // keep the most recent
			}
			for _, cm := range raw {
				cc := Comment{Body: truncateRunes(cm.Body, maxCommentBody), At: cm.CreatedAt}
				if cm.User != nil {
					cc.Author = cm.User.Login
				}
				d.Comments = append(d.Comments, cc)
			}
		}
	}

	// Changed file paths for PRs are best-effort and low-risk (paths, not patch).
	if isPR {
		if body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, number)); err == nil {
			var files []struct {
				Filename string `json:"filename"`
			}
			if err := json.Unmarshal(body, &files); err == nil {
				for i, f := range files {
					if i >= maxChangedFiles {
						break
					}
					d.ChangedFiles = append(d.ChangedFiles, f.Filename)
				}
			}
		}
	}

	return d, nil
}

// truncateRunes caps s to at most max bytes without splitting a UTF-8 rune,
// appending a marker when it trims.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	t := s[:max]
	for len(t) > 0 && !utf8.ValidString(t) {
		t = t[:len(t)-1]
	}
	return t + "… (truncated)"
}

// Read-only lookups for the conversation tools (docs/adr/0020). They use the
// user's vended credential and only ever GET; access is bounded by the token's
// own scopes (a repo it cannot see returns an error the caller surfaces).
const (
	maxFileBytes     = 32 << 10 // decoded file text returned to the model
	maxSearchResults = 20       // search matches returned to the model
)

// FileContents reads a file from a repo at an optional ref (branch/tag/SHA;
// empty uses the default branch) and returns its decoded text, bounded. A path
// that resolves to a directory, a binary blob, or an unreadable object returns a
// short explanatory note rather than an error, so the assistant can react.
func (c *Client) FileContents(ctx context.Context, token, repo, path, ref string) (string, error) {
	p := fmt.Sprintf("/repos/%s/contents/%s", repo, path)
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	body, err := c.get(ctx, token, p)
	if err != nil {
		return "", err
	}
	// A file responds with an object; a directory responds with a JSON array.
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		return "(path is a directory, not a file)", nil
	}
	if file.Type != "file" || file.Encoding != "base64" {
		return "(not a readable text file)", nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode file contents: %w", err)
	}
	if !utf8.Valid(raw) {
		return "(binary file omitted)", nil
	}
	return truncateRunes(string(raw), maxFileBytes), nil
}

// PullRequestStatus returns a pull request's current state (open/closed, merged
// or not, base/head, title) as a compact text summary.
func (c *Client) PullRequestStatus(ctx context.Context, token, repo string, number int) (string, error) {
	body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d", repo, number))
	if err != nil {
		return "", err
	}
	var pr struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		Merged   bool       `json:"merged"`
		MergedAt *time.Time `json:"merged_at"`
		Title    string     `json:"title"`
		Base     struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", err
	}
	merged := "not merged"
	if pr.Merged {
		merged = "merged"
		if pr.MergedAt != nil {
			merged += " at " + pr.MergedAt.UTC().Format(time.RFC3339)
		}
	}
	return fmt.Sprintf("PR %s#%d %q: state=%s, %s, base=%s, head=%s\n%s",
		repo, pr.Number, pr.Title, pr.State, merged, pr.Base.Ref, pr.Head.Ref, pr.HTMLURL), nil
}

// IssueStatus returns an issue's (or PR's) current state as a compact summary.
func (c *Client) IssueStatus(ctx context.Context, token, repo string, number int) (string, error) {
	body, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return "", err
	}
	var is struct {
		Number      int        `json:"number"`
		State       string     `json:"state"`
		Title       string     `json:"title"`
		ClosedAt    *time.Time `json:"closed_at"`
		HTMLURL     string     `json:"html_url"`
		PullRequest *struct{}  `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &is); err != nil {
		return "", err
	}
	kind := "issue"
	if is.PullRequest != nil {
		kind = "pull request"
	}
	closed := ""
	if is.ClosedAt != nil {
		closed = ", closed at " + is.ClosedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s %s#%d %q: state=%s%s\n%s", kind, repo, is.Number, is.Title, is.State, closed, is.HTMLURL), nil
}

// Search runs a GitHub issues/PRs search and returns the top matches as compact
// text. It accepts the full GitHub search syntax (e.g. "repo:owner/name otel").
func (c *Client) Search(ctx context.Context, token, query string) (string, error) {
	items, err := c.searchIssues(ctx, token, query)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "no matching issues or pull requests", nil
	}
	var b strings.Builder
	shown := len(items)
	if shown > maxSearchResults {
		shown = maxSearchResults
	}
	fmt.Fprintf(&b, "%d result(s) (showing %d):\n", len(items), shown)
	for _, it := range items[:shown] {
		repo := strings.TrimPrefix(it.RepositoryURL, defaultBaseURL+"/repos/")
		kind := "issue"
		if it.PullRequest != nil {
			kind = "PR"
		}
		fmt.Fprintf(&b, "- %s %s#%d %q [%s] %s\n", kind, repo, it.Number, it.Title, it.State, it.HTMLURL)
	}
	return b.String(), nil
}
