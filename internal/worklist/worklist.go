// Package worklist models the user's ordered set of work — GitHub issues and
// PRs enriched with Zumble-Zay metadata — plus the persistence and ingestion
// seams behind it.
//
// Ordering depends on Zumble-Zay metadata that cannot be inferred from the
// GitHub item alone, so sorting happens server-side (see sort.go).
package worklist

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a requested work item does not exist.
var ErrNotFound = errors.New("work item not found")

// Priority is a coarse, ZZ-assigned importance band.
type Priority string

const (
	PriorityNone   Priority = ""
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
)

// ItemType distinguishes the kind of work item.
type ItemType string

const (
	TypeIssue       ItemType = "issue"
	TypePullRequest ItemType = "pull_request"
)

// Reason records why an item is on the user's radar. An item may surface for
// several reasons at once (e.g. authored and review-requested); they are the
// raw relationship facts that feed the Relevance and Urgency axes.
type Reason string

const (
	ReasonReviewRequested Reason = "review_requested"
	ReasonAssignee        Reason = "assignee"
	ReasonAuthor          Reason = "author"
	ReasonMentioned       Reason = "mentioned"
	ReasonTeamMentioned   Reason = "team_mentioned"
	ReasonCodeowner       Reason = "codeowner"
)

// Origin records who set the metadata, so human overrides outrank agent values.
const (
	OriginAgent = "agent"
	OriginUser  = "user"
)

// GitHubRef identifies the upstream GitHub item.
type GitHubRef struct {
	Number    int       `json:"number"`
	Repo      string    `json:"repo"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Metadata is ZZ's judgment about an item, derived from its Signals. The four
// axes are orthogonal, each normalized 0..1 and independently sortable; Rank is
// their weighted blend (the default "most important first"). It cannot be
// derived from the GitHub item alone (see docs/adr/0008).
type Metadata struct {
	// Score axes (0..1).
	Relevance  float64 `json:"relevance"`  // closeness to me / my active work
	Impact     float64 `json:"impact"`     // strategic / org importance
	Engagement float64 `json:"engagement"` // social heat: level + velocity
	Urgency    float64 `json:"urgency"`    // time pressure / someone blocked on me
	Rank       float64 `json:"rank"`       // weighted blend of the axes

	// Priority is a coarse, human-facing band derived from Rank.
	Priority Priority `json:"priority"`

	// Contributions explain the score: which signals drove which axis, so the
	// Rationale is derived rather than authored.
	Contributions []Contribution `json:"contributions,omitempty"`
	Rationale     string         `json:"rationale,omitempty"`

	Origin    string    `json:"origin"` // OriginAgent | OriginUser
	ScoredAt  time.Time `json:"scored_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Contribution is one explainable factor in a score: a signal's signed weight
// toward an axis, with a human-readable detail for the UI.
type Contribution struct {
	Axis   string  `json:"axis"`   // relevance | impact | engagement | urgency
	Signal string  `json:"signal"` // e.g. "review_requested", "comment_accel"
	Weight float64 `json:"weight"` // signed contribution
	Detail string  `json:"detail,omitempty"`
}

// WorkItem is one unit of work owned by a user.
type WorkItem struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	Source    string    `json:"source"` // "github"
	Type      ItemType  `json:"type"`
	GitHub    GitHubRef `json:"github"`
	Signals   Signals   `json:"signals"` // observed facts that feed scoring
	Meta      Metadata  `json:"zz"`      // ZZ's judgment derived from Signals
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Signals are the observed FACTS about an item — measured, not judged. They are
// the inputs to scoring and are kept verbatim so an item can be re-scored when
// the weighting changes, and so any score can be explained (see docs/adr/0008).
// Producers may write different fields on different cadences (the GitHub agent
// fills engagement/temporal facts; a WorkIQ agent fills DiffuseInterest), so
// ObservedAt records the freshness of the measurement.
type Signals struct {
	// Relationship to the acting user (why it's on the radar).
	Reasons       []Reason `json:"reasons,omitempty"`        // review_requested | assignee | author | ...
	RelatedActive []string `json:"related_active,omitempty"` // IDs of my active items this overlaps

	// Engagement / heat.
	Comments          int     `json:"comments"`
	Participants      int     `json:"participants"`
	Reactions         int     `json:"reactions"`
	InfluentialActors int     `json:"influential_actors"` // distinct maintainers/TOC/SIG-leads engaged
	InboundRefs       int     `json:"inbound_refs"`       // other issues/PRs linking here (hub centrality)
	CommentVelocity   float64 `json:"comment_velocity"`   // comments/day, smoothed
	CommentAccel      float64 `json:"comment_accel"`      // Δ velocity vs prior window (the heatmap derivative)

	// Temporal.
	OpenedAt        time.Time `json:"opened_at"`
	LastActivityAt  time.Time `json:"last_activity_at"`
	AwaitingMeSince time.Time `json:"awaiting_me_since"` // zero = no outstanding ask on me
	DeadlineAt      time.Time `json:"deadline_at"`       // release/freeze/KEP; zero = none
	Reopened        bool      `json:"reopened"`

	// Strategic context.
	RepoTier      int      `json:"repo_tier"` // config-driven strategic weight bucket
	Labels        []string `json:"labels,omitempty"`
	Blocking      int      `json:"blocking"` // count of items this blocks
	RoadmapThemes []string `json:"roadmap_themes,omitempty"`

	// Soft / external (probabilistic; never treated as ground truth).
	Topics          []string `json:"topics,omitempty"`
	TrendScore      float64  `json:"trend_score"`      // 0..1 external trend strength
	DiffuseInterest float64  `json:"diffuse_interest"` // 0..1 WorkIQ partner-team interest

	// Proposed holds an LLM-proposed set of axes for this item. It is an INPUT
	// that ZZ ratifies against the deterministic baseline (docs/adr/0011), not a
	// final score; nil when no ranker has run.
	Proposed *AxisProposal `json:"proposed,omitempty"`

	ObservedAt time.Time `json:"observed_at"` // freshness of this measurement
}

// Store is the owner-scoped persistence contract for work items. The cloud
// persistence backend will implement this without changing callers.
type Store interface {
	List(ctx context.Context, ownerID string) ([]WorkItem, error)
	// Upsert adds or replaces an owner's items, keyed by WorkItem.ID. It is the
	// write side used by agent ingestion; implementations scope every item to
	// ownerID so an agent cannot write another user's data.
	Upsert(ctx context.Context, ownerID string, items ...WorkItem) error
}

// Ingestor starts the serialized agentic flow that backfills a user's work
// items: (1) retrieve raw GitHub data and populate items with default
// metadata, then (2) run parallel analysis agents that decorate the ZZ
// metadata and may create new items.
//
// Implementations MUST be idempotent (a duplicate request while a backfill is
// already in flight is a no-op) and MUST return promptly — EnsureBackfill is
// invoked from the request path when a work list is empty, so it should
// enqueue work rather than block on it.
type Ingestor interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
}
