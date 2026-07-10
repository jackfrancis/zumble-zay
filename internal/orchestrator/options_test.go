package orchestrator

import "testing"

// New honors WithWorkers/WithQueueDepth: the queue is sized to the override.
func TestNewHonorsWorkerAndQueueOptions(t *testing.T) {
	o := New(nil, NoopLauncher{}, nil, WithWorkers(7), WithQueueDepth(512))
	defer o.Stop()
	if got := cap(o.queue); got != 512 {
		t.Fatalf("queue cap = %d, want 512", got)
	}
}

// A non-positive override is ignored, so a misconfigured 0/negative keeps the
// safe built-in default rather than starting zero workers or a zero-length queue.
func TestWorkerAndQueueOptionsIgnoreNonPositive(t *testing.T) {
	c := orchestratorConfig{workers: defaultWorkers, queueDepth: queueDepth}
	WithWorkers(0)(&c)
	WithQueueDepth(-5)(&c)
	if c.workers != defaultWorkers || c.queueDepth != queueDepth {
		t.Fatalf("non-positive override applied: %+v", c)
	}
}
