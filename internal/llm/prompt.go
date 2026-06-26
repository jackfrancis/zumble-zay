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
- relevance: how much this item is the user's responsibility right now (a review explicitly requested of them, their own PR, an assignment) vs incidental.
- impact: how consequential the underlying change is (release-blocking bugs, security, broad blast radius, important areas score high; trivial or cosmetic score low).
- engagement: how much active collaboration is happening (comments, distinct participants, reactions, cross-references).
- urgency: how time-sensitive it is (the user has been blocking others for a while, a deadline is near, or it is going stale).

Also return:
- confidence: 0.0 to 1.0, how sure you are given the limited signals. Be honest; low signal means low confidence.
- rationale: one short sentence explaining the scores.

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
