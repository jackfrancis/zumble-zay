package worklist

import (
	"context"
	"time"
)

// AxisProposal is an LLM-proposed set of the four ranking axes for one item,
// with a confidence and rationale. It is an INPUT to ZZ's deterministic blend:
// ZZ ratifies it against the signal-based baseline (confidence gate + deviation
// clamp) rather than trusting it verbatim, so attacker-influenced content cannot
// fully hijack ordering (see docs/adr/0011).
type AxisProposal struct {
	Relevance  float64 `json:"relevance"`
	Impact     float64 `json:"impact"`
	Engagement float64 `json:"engagement"`
	Urgency    float64 `json:"urgency"`
	Confidence float64 `json:"confidence"` // 0..1; below the floor falls back to baseline
	Rationale  string  `json:"rationale,omitempty"`
}

// AxisRanker proposes the four axes for an item. The real implementation calls
// an LLM from an agent runtime; ZZ core depends only on this interface so it
// never imports a model client (docs/adr/0006, 0011).
type AxisRanker interface {
	Propose(ctx context.Context, item WorkItem) (AxisProposal, error)
}

// StubRanker is a deterministic AxisRanker that proposes the signal-based
// baseline axes with full confidence. It makes the ranking pipeline exercisable
// before a real model is attached; ratifying its proposal is a no-op, so it
// never changes ordering.
type StubRanker struct {
	now func() time.Time
}

// NewStubRanker returns a StubRanker using the wall clock.
func NewStubRanker() *StubRanker { return &StubRanker{now: time.Now} }

// Propose returns the deterministic baseline axes as the proposal.
func (s *StubRanker) Propose(_ context.Context, item WorkItem) (AxisProposal, error) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	rel, imp, eng, urg := baselineAxes(item.Signals, now().UTC())
	return AxisProposal{
		Relevance:  rel,
		Impact:     imp,
		Engagement: eng,
		Urgency:    urg,
		Confidence: 1,
		Rationale:  "baseline (stub ranker)",
	}, nil
}

// ResearchAdjustment is the discussion-derived re-weighting of the four
// foundation axes (docs/adr/0022). Each multiplier scales the corresponding
// foundation axis: 1.0 leaves it unchanged, <1 dampens, >1 amplifies. Multipliers
// are bounded to [0,2] and the product to [0,1] when Score applies them. It is
// the "research" layer: the GitHub metadata is the authoritative foundation, and
// this expresses how the conversation's *evidence* (never mere assertion)
// nuances it. The zero value is NOT neutral — 1.0 is; producers must set all four
// (the LLM parser defaults an absent multiplier to 1.0).
type ResearchAdjustment struct {
	Relevance  float64   `json:"relevance"`
	Impact     float64   `json:"impact"`
	Engagement float64   `json:"engagement"`
	Urgency    float64   `json:"urgency"`
	Rationale  string    `json:"rationale,omitempty"`
	AppliedAt  time.Time `json:"applied_at"`
}

// ResearchRanker proposes the research re-weighting for an item from its
// conversation thread, layered on the foundation proposal. The real
// implementation calls an LLM from an agent runtime; ZZ core depends only on this
// interface so it never imports a model client (docs/adr/0006, 0022).
type ResearchRanker interface {
	Research(ctx context.Context, item WorkItem) (ResearchAdjustment, error)
}
