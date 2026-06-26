package worklist

import (
	"context"
	"testing"
	"time"
)

func TestScoreRatifiesConfidentProposalWithinClamp(t *testing.T) {
	now := time.Now().UTC()
	base := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}}}, now)

	// Impact baseline is 0; a confident proposal of 0.3 is within the clamp.
	prop := &AxisProposal{Relevance: 0.6, Impact: 0.3, Confidence: 0.9, Rationale: "touches core"}
	got := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, Proposed: prop}}, now)

	if got.Impact <= base.Impact {
		t.Errorf("confident proposal should raise impact: base=%v got=%v", base.Impact, got.Impact)
	}
	if got.Impact != 0.3 {
		t.Errorf("impact = %v, want 0.3 (within clamp)", got.Impact)
	}
	if got.Rationale != "touches core" {
		t.Errorf("rationale = %q, want the LLM rationale", got.Rationale)
	}
}

func TestScoreClampsProposalDeviation(t *testing.T) {
	now := time.Now().UTC()
	// Impact baseline 0; proposal 1.0 is clamped to baseline + maxDeviation (0.4).
	prop := &AxisProposal{Impact: 1.0, Confidence: 1.0}
	got := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, Proposed: prop}}, now)

	if got.Impact != axisMaxDeviation {
		t.Errorf("impact = %v, want %v (clamped to baseline+maxDeviation)", got.Impact, axisMaxDeviation)
	}
}

func TestScoreLowConfidenceFallsBackToBaseline(t *testing.T) {
	now := time.Now().UTC()
	base := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}}}, now)

	prop := &AxisProposal{Impact: 1.0, Confidence: 0.2} // below the floor
	got := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, Proposed: prop}}, now)

	if got.Impact != base.Impact {
		t.Errorf("low-confidence proposal must be ignored: base=%v got=%v", base.Impact, got.Impact)
	}
}

func TestStubRankerProposesBaselineAndRatifiesToNoOp(t *testing.T) {
	now := time.Now().UTC()
	sig := Signals{Reasons: []Reason{ReasonReviewRequested}, Comments: 12, Participants: 6}

	p, err := NewStubRanker().Propose(context.Background(), WorkItem{Signals: sig})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	rel, imp, eng, urg := baselineAxes(sig, now)
	if p.Relevance != rel || p.Impact != imp || p.Engagement != eng || p.Urgency != urg {
		t.Errorf("stub should propose baseline axes; got %+v want (%v %v %v %v)", p, rel, imp, eng, urg)
	}
	if p.Confidence != 1 {
		t.Errorf("confidence = %v, want 1", p.Confidence)
	}

	// Ratifying the stub's proposal must not change the score.
	base := Score(WorkItem{Signals: sig}, now)
	withProp := sig
	withProp.Proposed = &p
	got := Score(WorkItem{Signals: withProp}, now)
	if got.Rank != base.Rank {
		t.Errorf("stub ratification should be a no-op: base rank=%v got=%v", base.Rank, got.Rank)
	}
}
