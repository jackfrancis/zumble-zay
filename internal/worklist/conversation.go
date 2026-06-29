package worklist

import (
	"context"
	"encoding/json"
	"time"
)

// Conversation roles. A thread alternates user and agent turns.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Message is one turn in an item's assistive conversation thread, retained on
// the WorkItem (docs/adr/0018). Role is RoleUser or RoleAgent.
type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

// ToolDef describes a read-only tool the assistant may call during a turn
// (docs/adr/0020). Parameters is a JSON Schema object for the tool's arguments.
// It is provider-neutral: the runtime supplies concrete tools (e.g. GitHub
// reads) behind it, so ZZ core imports no provider client (docs/adr/0006).
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolBox is the set of read-only tools available to a Conversationalist for one
// turn. The runtime implements it over a provider client and the user's vended
// credential; a nil ToolBox means no tools, so the assistant reasons over the
// item and thread alone. Invoke runs a named tool with JSON arguments and
// returns a text result for the model; it must never perform a write.
type ToolBox interface {
	Definitions() []ToolDef
	Invoke(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// Conversationalist produces an assistant reply for an item, given freshly
// gathered source context, the prior thread, the user's new message, and a set
// of read-only tools it may call to look up live data. It is **read-only and
// advisory**: it may summarize, draft, or suggest, but ZZ never acts on GitHub
// from it (docs/adr/0018). The real implementation calls an LLM from outside ZZ
// core, behind this interface, so no core package imports a model client
// (docs/adr/0006, 0011).
//
// sourceContext is additional, freshly fetched provider content (e.g. a PR's
// description, discussion, and changed files) that the converse runtime gathered
// with a ZZ-vended credential (docs/adr/0019). It is UNTRUSTED, attacker-
// influenceable data; implementations must frame it as data, never instructions.
// It may be empty (no credential, a fetch failure, or an in-process turn), in
// which case the assistant reasons only over the item's existing ZZ metadata.
//
// tools may be nil; when present, the assistant can call them to verify claims
// against live GitHub data before answering (docs/adr/0020). They are read-only.
type Conversationalist interface {
	Reply(ctx context.Context, item WorkItem, sourceContext string, history []Message, userText string, tools ToolBox) (string, error)
}
