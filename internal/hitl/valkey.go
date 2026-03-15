package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	vk "github.com/valkey-io/valkey-go"
)

// ValkeyStore persists pending HITL interrupts in Valkey/Redis.
type ValkeyStore struct {
	client vk.Client
	prefix string
	ttl    time.Duration
}

// NewValkeyStore creates a ValkeyStore using an existing valkey.Client.
// Prefix defaults to "hitl:" and TTL defaults to 1 hour if zero.
func NewValkeyStore(client vk.Client, prefix string, ttl time.Duration) *ValkeyStore {
	if prefix == "" {
		prefix = "hitl:"
	}
	if ttl == 0 {
		ttl = time.Hour
	}
	return &ValkeyStore{
		client: client,
		prefix: prefix,
		ttl:    ttl,
	}
}

func (s *ValkeyStore) key(threadID string) string {
	return s.prefix + threadID
}

func (s *ValkeyStore) Set(threadID string, p *PendingInterrupt) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("hitl valkey set: marshal: %w", err)
	}
	return s.client.Do(context.Background(), s.client.B().Set().Key(s.key(threadID)).Value(string(data)).Ex(s.ttl).Build()).Error()
}

func (s *ValkeyStore) Get(threadID string) (*PendingInterrupt, error) {
	data, err := s.client.Do(context.Background(), s.client.B().Get().Key(s.key(threadID)).Build()).AsBytes()
	if err != nil {
		if vk.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("hitl valkey get: %w", err)
	}
	var p PendingInterrupt
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("hitl valkey get: unmarshal: %w", err)
	}
	return &p, nil
}

func (s *ValkeyStore) Clear(threadID string) error {
	return s.client.Do(context.Background(), s.client.B().Del().Key(s.key(threadID)).Build()).Error()
}
