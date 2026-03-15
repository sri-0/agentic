package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore persists pending HITL interrupts in Redis.
type RedisStore struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

// NewRedisStore creates a RedisStore using an existing *redis.Client.
// Prefix defaults to "hitl:" and TTL defaults to 1 hour if zero.
func NewRedisStore(client *redis.Client, prefix string, ttl time.Duration) *RedisStore {
	if prefix == "" {
		prefix = "hitl:"
	}
	if ttl == 0 {
		ttl = time.Hour
	}
	return &RedisStore{
		client: client,
		prefix: prefix,
		ttl:    ttl,
	}
}

func (s *RedisStore) key(threadID string) string {
	return s.prefix + threadID
}

func (s *RedisStore) Set(threadID string, p *PendingInterrupt) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("hitl redis set: marshal: %w", err)
	}
	return s.client.Set(context.Background(), s.key(threadID), data, s.ttl).Err()
}

func (s *RedisStore) Get(threadID string) (*PendingInterrupt, error) {
	data, err := s.client.Get(context.Background(), s.key(threadID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hitl redis get: %w", err)
	}
	var p PendingInterrupt
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("hitl redis get: unmarshal: %w", err)
	}
	return &p, nil
}

func (s *RedisStore) Clear(threadID string) error {
	return s.client.Del(context.Background(), s.key(threadID)).Err()
}
