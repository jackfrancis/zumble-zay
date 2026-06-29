package llm

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

// systemPrompt defines the task and the four axes so the model scores
// consistently with ZZ's signal-based baseline (docs/adr/0011). ZZ ratifies the
// output against that baseline, so the prompt aims for calibrated, honest scores
// rather than trusting the model blindly.
const systemPrompt = `You rank a software engineer's GitHub work items for a personal "what needs my attention" radar.

Score the item on four axes, each a number from 0.0 to 1.0:
- relevance: how much this item needs the user's OWN attention right now. High when a review was explicitly requested of them, it is their own PR, or their username is directly mentioned (they are wanted specifically). A bare assignment they have not yet engaged with (no requested review, no mention) is only MEDIUM. It drops to LOW when "others_reviewing" is set (other reviewers are already handling it) or "waiting_on_others" is set (the user has already acted, or a review has landed and progress is on the author or a third party) — it is not actionable by the user right now. A direct mention keeps relevance high even when others are engaged.
- impact: how consequential the underlying change is (release-blocking bugs, security, broad blast radius, important areas score high; trivial or cosmetic score low).
- engagement: how much active collaboration is happening (comments, distinct participants, reactions, cross-references).
- urgency: how time-sensitive it is FOR THE USER (they have been blocking others for a while, a deadline is near, or it is going stale). If "waiting_on_others" is set, the ball is not in the user's court and there is no time pressure on them, so urgency is ~0. If "others_reviewing" is set and no review was requested of the user, urgency is low — someone else is driving it.

Also return:
- confidence: 0.0 to 1.0, how sure you are given the limited signals. Be honest; low signal means low confidence.
- rationale: one short sentence explaining the scores, addressed directly to the reader in the SECOND PERSON ("you"/"your"). Never write "the user", "they", or "their" — the reader is the engineer whose radar this is.

Respond with ONLY a JSON object, no prose, with exactly these keys:
{"relevance":0.0,"impact":0.0,"engagement":0.0,"urgency":0.0,"confidence":0.0,"rationale":"..."}`

// userPrompt serializes the item's observable signals into a compact JSON
// object for the model. Only facts ZZ already holds are sent; the runtime does
// not fetch anything extra for ranking.
func userPrompt(item worklist.WorkItem) string {
	now := time.Now().UTC()
	reasons := make([]string, 0, len(item.Signals.Reasons))
	for _, r := range item.Signals.Reasons {
		reasons = append(reasons, string(r))
	}

	summary := map[string]any{
		"repo":         item.GitHub.Repo,
		"type":         item.Type,
		"title":        item.GitHub.Title,
		"state":        item.GitHub.State,
		"reasons":      reasons,
		"labels":       item.Signals.Labels,
		"comments":     item.Signals.Comments,
		"participants": item.Signals.Participants,
		"reactions":    item.Signals.Reactions,
		"inbound_refs": item.Signals.InboundRefs,
		"age_days":     daysSince(item.GitHub.UpdatedAt, now),
	}
	if !item.Signals.AwaitingMeSince.IsZero() {
		summary["awaiting_me_days"] = daysSince(item.Signals.AwaitingMeSince, now)
	}
	if !item.Signals.AwaitingOthersSince.IsZero() {
		// Forward progress is blocked on the author/others, not the user: the ball
		// is not in their court, so urgency and present relevance are low
		// (docs/adr/0015).
		summary["waiting_on_others"] = true
		summary["awaiting_others_days"] = daysSince(item.Signals.AwaitingOthersSince, now)
	}
	if item.Signals.OtherReviewers > 0 {
		// Someone else is already reviewing: the user's own attention is less
		// needed unless they were specifically asked or mentioned (docs/adr/0015).
		summary["others_reviewing"] = item.Signals.OtherReviewers
	}
	if !item.Signals.DeadlineAt.IsZero() {
		summary["deadline_in_days"] = daysSince(now, item.Signals.DeadlineAt)
	}

	b, err := json.Marshal(summary)
	if err != nil {
		// summary holds only plain scalars/slices, so this is unreachable; fall
		// back to a minimal prompt rather than failing the rank.
		return fmt.Sprintf("Score this GitHub %s in %s.", item.Type, item.GitHub.Repo)
	}
	return "Score this GitHub work item:\n" + string(b)
}

// daysSince returns whole days between two times, never negative.
func daysSince(from, to time.Time) int {
	d := to.Sub(from).Hours() / 24
	if d < 0 {
		return 0
	}
	return int(d)
}
