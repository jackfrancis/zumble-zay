package worklist

import (
	"context"
	"testing"
	"time"
)

func TestScoreUsesProposalAxesAuthoritatively(t *testing.T) {
	now := time.Now().UTC()
	base := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}}}, now)

	// The proposal is used verbatim for the axes it returns, regardless of the
	// baseline (impact baseline is 0 here; the proposal sets 0.3).
	prop := &AxisProposal{Relevance: 0.6, Impact: 0.3, Confidence: 0.9, Rationale: "touches core"}
	got := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, Proposed: prop}}, now)

	if got.Impact <= base.Impact {
		t.Errorf("proposal should raise impact: base=%v got=%v", base.Impact, got.Impact)
	}
	if got.Impact != 0.3 || got.Relevance != 0.6 {
		t.Errorf("axes = (rel %v, imp %v), want the proposed (0.6, 0.3)", got.Relevance, got.Impact)
	}
	if got.Rationale != "touches core" {
		t.Errorf("rationale = %q, want the LLM rationale", got.Rationale)
	}
}

func TestScoreProposalIsAuthoritativeRegardlessOfConfidenceOrDeviation(t *testing.T) {
	now := time.Now().UTC()
	// Impact baseline is 0; a far-from-baseline, low-confidence proposal still
	// passes through unchanged: there is no deviation clamp and no confidence
	// floor (docs/adr/0015).
	prop := &AxisProposal{Impact: 1.0, Confidence: 0.2}
	got := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, Proposed: prop}}, now)

	if got.Impact != 1.0 {
		t.Errorf("impact = %v, want 1.0 (authoritative pass-through)", got.Impact)
	}
}

func TestScoreClampsProposalToUnitRange(t *testing.T) {
	now := time.Now().UTC()
	prop := &AxisProposal{Relevance: 1.5, Impact: -0.2, Engagement: 0.5, Urgency: 2.0, Confidence: 1.0}
	got := Score(WorkItem{Signals: Signals{Proposed: prop}}, now)

	if got.Relevance != 1.0 || got.Impact != 0.0 || got.Urgency != 1.0 || got.Engagement != 0.5 {
		t.Errorf("axes not bounded to [0,1]: %+v", got)
	}
}

func TestStubRankerProposesBaselineAndScoresToNoOp(t *testing.T) {
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

	// The stub proposes the baseline, so scoring with it must match the baseline.
	base := Score(WorkItem{Signals: sig}, now)
	withProp := sig
	withProp.Proposed = &p
	got := Score(WorkItem{Signals: withProp}, now)
	if got.Rank != base.Rank {
		t.Errorf("stub proposal should be a no-op: base rank=%v got=%v", base.Rank, got.Rank)
	}
}
