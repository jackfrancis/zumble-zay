package worklist

import (
	"testing"
	"time"
)

func TestHasUnreadReply(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	user := Message{Role: RoleUser, Content: "hi", At: base}
	agent := Message{Role: RoleAgent, Content: "reply", At: base.Add(time.Minute)}

	cases := []struct {
		name   string
		thread []Message
		readAt time.Time
		want   bool
	}{
		{"empty thread", nil, time.Time{}, false},
		{"reply never read", []Message{user, agent}, time.Time{}, true},
		{"reply read after", []Message{user, agent}, agent.At.Add(time.Second), false},
		{"reply read before is unread", []Message{user, agent}, base, true},
		{"pending user turn is not unread", []Message{user}, time.Time{}, false},
	}
	for _, tc := range cases {
		it := WorkItem{Thread: tc.thread, ThreadReadAt: tc.readAt}
		if got := it.HasUnreadReply(); got != tc.want {
			t.Errorf("%s: HasUnreadReply()=%v want %v", tc.name, got, tc.want)
		}
	}
}
