package worklist

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Scoring weights blend the four axes into Rank. They are deliberate, hand-set
// placeholders; ADR 0008 moves them to configuration once tuned.
const (
	wRelevance  = 0.35
	wUrgency    = 0.30
	wImpact     = 0.20
	wEngagement = 0.15
)

// Score computes an item's ZZ Metadata from its Signals, as of now. It is a
// pure function of the item, so a worklist can be re-scored when the weights
// change without re-fetching from the provider (see docs/adr/0008). Origin is
// preserved; the caller owns persistence timestamps.
//
// Time-dependent axes (Engagement, Urgency) are evaluated against now, so a
// later read-time rescore keeps them fresh as the ADR anticipates.
func Score(item WorkItem, now time.Time) Metadata {
	var contribs []Contribution

	rel := relevanceScore(item.Signals, &contribs)
	urg := urgencyScore(item.Signals, now, &contribs)
	eng := engagementScore(item.Signals, &contribs)
	imp := impactScore(item.Signals, &contribs)

	rank := wRelevance*rel + wUrgency*urg + wImpact*imp + wEngagement*eng

	m := Metadata{
		Relevance:     rel,
		Impact:        imp,
		Engagement:    eng,
		Urgency:       urg,
		Rank:          rank,
		Priority:      band(rank),
		Contributions: contribs,
		Origin:        item.Meta.Origin,
		ScoredAt:      now,
	}
	m.Rationale = rationale(contribs)
	return m
}

// relevanceScore: the strongest relationship reason wins, with a small bump for
// corroborating reasons.
func relevanceScore(s Signals, c *[]Contribution) float64 {
	weights := map[Reason]float64{
		ReasonReviewRequested: 1.0,
		ReasonAssignee:        0.9,
		ReasonCodeowner:       0.8,
		ReasonAuthor:          0.6,
		ReasonMentioned:       0.5,
		ReasonTeamMentioned:   0.4,
	}
	var best float64
	var bestReason Reason
	for _, r := range s.Reasons {
		if w := weights[r]; w > best {
			best, bestReason = w, r
		}
	}
	if best == 0 {
		return 0
	}
	if n := len(s.Reasons); n > 1 {
		best = math.Min(1, best+0.05*float64(n-1))
	}
	add(c, "relevance", string(bestReason), best, "on your radar as "+humanReason(bestReason))
	return best
}

// urgencyScore: an outstanding ask on the user, a review request, or an
// approaching deadline all create time pressure; the strongest wins.
func urgencyScore(s Signals, now time.Time, c *[]Contribution) float64 {
	var urg float64

	if !s.AwaitingMeSince.IsZero() {
		days := now.Sub(s.AwaitingMeSince).Hours() / 24
		u := math.Min(1, 0.6+0.1*days) // ramps as it ages
		if u > urg {
			urg = u
		}
		add(c, "urgency", "awaiting_me", u, "awaiting your response since "+s.AwaitingMeSince.Format("Jan 2"))
	}

	if containsReason(s.Reasons, ReasonReviewRequested) && urg < 0.6 {
		urg = 0.6
		add(c, "urgency", string(ReasonReviewRequested), 0.6, "review requested")
	}

	if !s.DeadlineAt.IsZero() {
		if u := deadlineUrgency(s.DeadlineAt, now); u > 0 {
			if u > urg {
				urg = u
			}
			add(c, "urgency", "deadline", u, "due "+s.DeadlineAt.Format("Jan 2"))
		}
	}
	return urg
}

// deadlineUrgency ramps from 0 at 30 days out to 1 at (or past) the due date.
func deadlineUrgency(due, now time.Time) float64 {
	d := due.Sub(now).Hours() / 24
	switch {
	case d <= 0:
		return 1
	case d >= 30:
		return 0
	default:
		return 1 - d/30
	}
}

// engagementScore: social heat from discussion level and velocity, each
// saturating so a runaway thread cannot dominate the blend.
func engagementScore(s Signals, c *[]Contribution) float64 {
	comments := saturate(float64(s.Comments), 10)
	reactions := saturate(float64(s.Reactions), 20)
	velocity := saturate(s.CommentVelocity, 5)
	eng := math.Min(1, 0.6*comments+0.25*reactions+0.15*velocity)
	if eng > 0 {
		add(c, "engagement", "discussion", eng,
			fmt.Sprintf("%d comments, %d reactions", s.Comments, s.Reactions))
	}
	return eng
}

// impactScore: strategic importance from labels and repo tier; strongest wins.
func impactScore(s Signals, c *[]Contribution) float64 {
	var imp float64
	var detail string
	for _, l := range s.Labels {
		if w, name := labelImpact(l); w > imp {
			imp, detail = w, name
		}
	}
	if s.RepoTier > 0 {
		if t := math.Min(1, float64(s.RepoTier)/3); t > imp {
			imp, detail = t, "strategic repo"
		}
	}
	if imp > 0 {
		add(c, "impact", "label", imp, detail)
	}
	return imp
}

// labelImpact maps a GitHub label to a strategic-importance weight.
func labelImpact(label string) (float64, string) {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "security"), strings.Contains(l, "vuln"):
		return 1.0, "security"
	case strings.Contains(l, "release-blocker"), strings.Contains(l, "critical"):
		return 0.9, l
	case strings.HasPrefix(l, "priority/p0"), strings.HasPrefix(l, "priority/p1"),
		strings.Contains(l, "priority/important"):
		return 0.75, l
	case strings.HasPrefix(l, "kind/bug"):
		return 0.6, "bug"
	default:
		return 0, ""
	}
}

func band(rank float64) Priority {
	switch {
	case rank >= 0.66:
		return PriorityHigh
	case rank >= 0.33:
		return PriorityMedium
	case rank > 0:
		return PriorityLow
	default:
		return PriorityNone
	}
}

func saturate(x, k float64) float64 {
	if x <= 0 {
		return 0
	}
	return x / (x + k)
}

func add(c *[]Contribution, axis, signal string, weight float64, detail string) {
	*c = append(*c, Contribution{Axis: axis, Signal: signal, Weight: weight, Detail: detail})
}

// rationale leads with the highest-weight contribution's human-readable detail.
func rationale(c []Contribution) string {
	if len(c) == 0 {
		return ""
	}
	top := c[0]
	for _, x := range c[1:] {
		if x.Weight > top.Weight {
			top = x
		}
	}
	return top.Detail
}

func containsReason(rs []Reason, r Reason) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func humanReason(r Reason) string {
	switch r {
	case ReasonReviewRequested:
		return "a review request"
	case ReasonAssignee:
		return "assigned to you"
	case ReasonCodeowner:
		return "code you own"
	case ReasonAuthor:
		return "authored by you"
	case ReasonMentioned:
		return "a mention"
	case ReasonTeamMentioned:
		return "a team mention"
	default:
		return string(r)
	}
}
