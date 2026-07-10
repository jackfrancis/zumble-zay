package llm

import (
	"context"
	"sync/atomic"
	"time"
)

// Collector accumulates a job's chat-model timing and tool-use counts so a
// runtime can report where its wall clock went (docs/adr/0024). It rides on the
// context, so the chat primitive and the converse tool loop record into it
// without threading a parameter through the Ranker/Converser seams. All methods
// are safe for concurrent use — the ranker fans model calls out in parallel.
type Collector struct {
	modelNanos atomic.Int64
	modelCalls atomic.Int64
	toolCalls  atomic.Int64
}

// ModelDuration is the summed wall time of the chat-model calls recorded so far.
func (c *Collector) ModelDuration() time.Duration { return time.Duration(c.modelNanos.Load()) }

// ModelCalls is the number of chat-model calls recorded so far.
func (c *Collector) ModelCalls() int { return int(c.modelCalls.Load()) }

// ToolCalls is the number of tool invocations recorded so far.
func (c *Collector) ToolCalls() int { return int(c.toolCalls.Load()) }

type collectorKey struct{}

// WithCollector returns a context carrying a fresh Collector plus the collector
// itself, so a runtime wraps the job context once and reads the totals after the
// job returns. Model calls and tool invocations made under the returned context
// are recorded automatically.
func WithCollector(ctx context.Context) (context.Context, *Collector) {
	c := &Collector{}
	return context.WithValue(ctx, collectorKey{}, c), c
}

func collectorFrom(ctx context.Context) *Collector {
	c, _ := ctx.Value(collectorKey{}).(*Collector)
	return c
}

// recordModelCall adds one chat-model call's wall time to the context collector
// if one is present; a nil collector (no runtime wrapping, e.g. unit tests) is a
// no-op, so the chat primitive can call it unconditionally.
func recordModelCall(ctx context.Context, d time.Duration) {
	if c := collectorFrom(ctx); c != nil {
		c.modelNanos.Add(int64(d))
		c.modelCalls.Add(1)
	}
}

// recordToolCall counts one tool invocation on the context collector, if present.
func recordToolCall(ctx context.Context) {
	if c := collectorFrom(ctx); c != nil {
		c.toolCalls.Add(1)
	}
}
