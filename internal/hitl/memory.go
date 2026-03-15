package hitl

import "sync"

// InMemoryStore is an in-memory Store backed by a sync.RWMutex map.
type InMemoryStore struct {
	mu      sync.RWMutex
	pending map[string]*PendingInterrupt
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		pending: make(map[string]*PendingInterrupt),
	}
}

func (s *InMemoryStore) Set(threadID string, p *PendingInterrupt) error {
	s.mu.Lock()
	s.pending[threadID] = p
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) Get(threadID string) (*PendingInterrupt, error) {
	s.mu.RLock()
	p := s.pending[threadID]
	s.mu.RUnlock()
	return p, nil
}

func (s *InMemoryStore) Clear(threadID string) error {
	s.mu.Lock()
	delete(s.pending, threadID)
	s.mu.Unlock()
	return nil
}
