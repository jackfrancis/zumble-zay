package llm

import (
	"strings"
	"testing"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

func TestUserPromptSurfacesWaitingOnOthers(t *testing.T) {
	item := worklist.WorkItem{
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "octo/repo", Title: "Fix it", Number: 1},
		Signals: worklist.Signals{
			Reasons:             []worklist.Reason{worklist.ReasonAssignee},
			AwaitingOthersSince: time.Now().Add(-72 * time.Hour),
		},
	}

	// When the ball is on the author, the prompt surfaces the flag so the model
	// can drive urgency (and present relevance) down (docs/adr/0015).
	p := userPrompt(item)
	if !strings.Contains(p, "waiting_on_others") || !strings.Contains(p, "awaiting_others_days") {
		t.Fatalf("prompt should surface the waiting-on-others signal, got:\n%s", p)
	}

	// Absent the signal, the flag is omitted.
	item.Signals.AwaitingOthersSince = time.Time{}
	if got := userPrompt(item); strings.Contains(got, "waiting_on_others") {
		t.Fatalf("prompt should omit waiting_on_others when not set, got:\n%s", got)
	}
}

func TestUserPromptSurfacesOthersReviewing(t *testing.T) {
	item := worklist.WorkItem{
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Repo: "octo/repo", Title: "Fix it", Number: 1},
		Signals: worklist.Signals{
			Reasons:        []worklist.Reason{worklist.ReasonAssignee},
			OtherReviewers: 2,
		},
	}

	// A bare assignee with other reviewers already engaged: surface the count so
	// the model can hold relevance/urgency down (docs/adr/0015).
	if p := userPrompt(item); !strings.Contains(p, "others_reviewing") {
		t.Fatalf("prompt should surface others_reviewing, got:\n%s", p)
	}

	// Absent the signal, the field is omitted.
	item.Signals.OtherReviewers = 0
	if got := userPrompt(item); strings.Contains(got, "others_reviewing") {
		t.Fatalf("prompt should omit others_reviewing when zero, got:\n%s", got)
	}
}
