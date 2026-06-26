package worklist

import (
	"testing"
	"time"
)

func TestScoreReviewRequestedIsActionable(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	item := WorkItem{Signals: Signals{Reasons: []Reason{ReasonReviewRequested}}}

	m := Score(item, now)

	if m.Relevance != 1.0 {
		t.Errorf("relevance = %v, want 1.0", m.Relevance)
	}
	if m.Urgency < 0.6 {
		t.Errorf("urgency = %v, want >= 0.6 (an open review request is an ask)", m.Urgency)
	}
	if m.Priority == PriorityNone {
		t.Errorf("priority should not be none for a review request")
	}
	if m.Rationale == "" || len(m.Contributions) == 0 {
		t.Errorf("expected explanation; rationale=%q contributions=%d", m.Rationale, len(m.Contributions))
	}
	if m.ScoredAt != now {
		t.Errorf("ScoredAt = %v, want %v", m.ScoredAt, now)
	}
}

func TestScoreSecurityLabelDrivesImpact(t *testing.T) {
	now := time.Now().UTC()
	item := WorkItem{Signals: Signals{
		Reasons: []Reason{ReasonAuthor},
		Labels:  []string{"area/net", "security/critical"},
	}}

	m := Score(item, now)

	if m.Impact != 1.0 {
		t.Errorf("impact = %v, want 1.0 for a security label", m.Impact)
	}
}

func TestScoreDeadlineProximityDrivesUrgency(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	soon := WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, DeadlineAt: now.Add(24 * time.Hour)}}
	farOff := WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, DeadlineAt: now.Add(60 * 24 * time.Hour)}}

	if got := Score(soon, now).Urgency; got < 0.9 {
		t.Errorf("urgency for a deadline tomorrow = %v, want >= 0.9", got)
	}
	if got := Score(farOff, now).Urgency; got != 0 {
		t.Errorf("urgency for a deadline 60 days out = %v, want 0", got)
	}
}

func TestScoreEngagementMonotonicAndPreservesOrigin(t *testing.T) {
	now := time.Now().UTC()
	quiet := WorkItem{Meta: Metadata{Origin: OriginAgent}, Signals: Signals{Reasons: []Reason{ReasonAuthor}}}
	busy := WorkItem{Meta: Metadata{Origin: OriginAgent}, Signals: Signals{
		Reasons: []Reason{ReasonAuthor}, Comments: 40, Reactions: 30,
	}}

	mq, mb := Score(quiet, now), Score(busy, now)
	if mb.Engagement <= mq.Engagement {
		t.Errorf("engagement should rise with discussion: quiet=%v busy=%v", mq.Engagement, mb.Engagement)
	}
	if mb.Rank <= mq.Rank {
		t.Errorf("rank should rise with engagement: quiet=%v busy=%v", mq.Rank, mb.Rank)
	}
	if mb.Origin != OriginAgent {
		t.Errorf("Score must preserve Origin, got %q", mb.Origin)
	}
}

func TestScoreUsesParticipantsAndInboundRefs(t *testing.T) {
	now := time.Now().UTC()
	base := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, Comments: 5}}, now)
	broad := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, Comments: 5, Participants: 8}}, now)
	if broad.Engagement <= base.Engagement {
		t.Errorf("participants should raise engagement: base=%v broad=%v", base.Engagement, broad.Engagement)
	}
	hub := Score(WorkItem{Signals: Signals{Reasons: []Reason{ReasonAuthor}, InboundRefs: 6}}, now)
	if hub.Impact == 0 {
		t.Errorf("inbound refs should produce impact, got 0")
	}
}

func TestScoreEmptySignalsIsInert(t *testing.T) {
	m := Score(WorkItem{}, time.Now().UTC())
	if m.Rank != 0 || m.Priority != PriorityNone {
		t.Errorf("empty signals should score zero: rank=%v priority=%q", m.Rank, m.Priority)
	}
	if len(m.Contributions) != 0 || m.Rationale != "" {
		t.Errorf("empty signals should have no explanation: contributions=%d rationale=%q", len(m.Contributions), m.Rationale)
	}
}
