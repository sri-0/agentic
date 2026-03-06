package agent

import "sync"

// PendingConfirmation holds state for a tool call awaiting human approval.
type PendingConfirmation struct {
	ToolCallID string
	ToolName   string
	Prompt     string
	Details    any
}

// HITLStore is a thread-safe store for pending human-in-the-loop confirmations.
type HITLStore struct {
	mu        sync.RWMutex
	pending   map[string]*PendingConfirmation // keyed by threadID
	decisions map[string]string               // keyed by threadID
}

func NewHITLStore() *HITLStore {
	return &HITLStore{
		pending:   make(map[string]*PendingConfirmation),
		decisions: make(map[string]string),
	}
}

// SetPending stores a pending confirmation for a thread.
func (s *HITLStore) SetPending(threadID string, pc *PendingConfirmation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[threadID] = pc
}

// GetPending returns the pending confirmation for a thread, if any.
func (s *HITLStore) GetPending(threadID string) *PendingConfirmation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pending[threadID]
}

// ClearPending removes the pending confirmation for a thread.
func (s *HITLStore) ClearPending(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, threadID)
}

// SetDecision stores a human decision for a thread.
func (s *HITLStore) SetDecision(threadID, decision string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions[threadID] = decision
}

// GetAndClearDecision atomically reads and removes a decision for a thread.
func (s *HITLStore) GetAndClearDecision(threadID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.decisions[threadID]
	delete(s.decisions, threadID)
	return d
}
