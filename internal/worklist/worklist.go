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

// Metadata is the Zumble-Zay-specific decoration that drives ordering. It
// cannot be derived from the GitHub item alone.
type Metadata struct {
	Priority  Priority  `json:"priority"`
	Relevance float64   `json:"relevance"`
	Impact    float64   `json:"impact"`
	Rank      float64   `json:"rank"`
	Origin    string    `json:"origin"` // OriginAgent | OriginUser
	Rationale string    `json:"rationale,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WorkItem is one unit of work owned by a user.
type WorkItem struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	Source    string    `json:"source"` // "github"
	Type      ItemType  `json:"type"`
	GitHub    GitHubRef `json:"github"`
	Meta      Metadata  `json:"zz"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the owner-scoped persistence contract for work items. The cloud
// persistence backend will implement this without changing callers.
type Store interface {
	List(ctx context.Context, ownerID string) ([]WorkItem, error)
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
