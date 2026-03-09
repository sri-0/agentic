package agent

import (
	"sync"

	"google.golang.org/genai"
)

type ConversationStore struct {
	mu    sync.RWMutex
	convs map[string][]*genai.Content
}

func NewConversationStore() *ConversationStore {
	return &ConversationStore{convs: make(map[string][]*genai.Content)}
}

func (s *ConversationStore) Get(threadID string) []*genai.Content {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.convs[threadID]
}

func (s *ConversationStore) Append(threadID string, contents ...*genai.Content) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.convs[threadID] = append(s.convs[threadID], contents...)
}

func (s *ConversationStore) Clear(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.convs, threadID)
}
