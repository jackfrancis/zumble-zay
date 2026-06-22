package worklist

import (
	"context"
	"sync"
)

// MemoryStore is an in-memory Store for development and tests. The cloud
// persistence backend will implement the same Store interface.
type MemoryStore struct {
	mu    sync.RWMutex
	items map[string][]WorkItem // ownerID -> items
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string][]WorkItem)}
}

// List returns a copy of the owner's work items.
func (s *MemoryStore) List(_ context.Context, ownerID string) ([]WorkItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.items[ownerID]
	out := make([]WorkItem, len(src))
	copy(out, src)
	return out, nil
}

// Seed replaces the items for an owner. Development and test helper.
func (s *MemoryStore) Seed(ownerID string, items ...WorkItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[ownerID] = items
}
