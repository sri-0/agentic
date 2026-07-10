package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	valkey "github.com/valkey-io/valkey-go"
)

// TaskBoardStore persists the CURRENT task board (the spawn-synthesised task list
// merged with todowrite snapshots) per session, so a reconnecting client can show
// the current board immediately without replaying the whole event log. The event
// log remains the source of truth (each snapshot is also an EvTaskList event);
// this is a fast-path cache of the latest snapshot keyed by session.
//
// Two implementations: a Redis/Valkey-backed store (the original ask — "task
// state in Redis") and an in-memory fallback used when Valkey isn't configured,
// mirroring the codebase's degrade-to-memory pattern.
type TaskBoardStore interface {
	// Set replaces the current board snapshot for a session.
	Set(ctx context.Context, sessionID string, tasks []TaskItem) error
	// Get returns the current board snapshot (empty slice if none).
	Get(ctx context.Context, sessionID string) ([]TaskItem, error)
}

// MemoryTaskBoardStore is the in-memory fallback TaskBoardStore.
type MemoryTaskBoardStore struct {
	mu     sync.RWMutex
	boards map[string][]TaskItem
}

// NewMemoryTaskBoardStore returns an in-memory task board store.
func NewMemoryTaskBoardStore() *MemoryTaskBoardStore {
	return &MemoryTaskBoardStore{boards: map[string][]TaskItem{}}
}

func (m *MemoryTaskBoardStore) Set(_ context.Context, sessionID string, tasks []TaskItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]TaskItem, len(tasks))
	copy(cp, tasks)
	m.boards[sessionID] = cp
	return nil
}

func (m *MemoryTaskBoardStore) Get(_ context.Context, sessionID string) ([]TaskItem, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b := m.boards[sessionID]
	out := make([]TaskItem, len(b))
	copy(out, b)
	return out, nil
}

// RedisTaskBoardStore persists the board to a per-session Redis key
// taskboard:{app}:{session} with a TTL (defaults 24h), matching the event log's
// hot window.
type RedisTaskBoardStore struct {
	client valkey.Client
	app    string
	ttl    time.Duration
}

// NewRedisTaskBoardStore wraps a valkey client as a TaskBoardStore.
func NewRedisTaskBoardStore(client valkey.Client, app string) *RedisTaskBoardStore {
	return &RedisTaskBoardStore{client: client, app: app, ttl: 24 * time.Hour}
}

func (r *RedisTaskBoardStore) key(sessionID string) string {
	return fmt.Sprintf("taskboard:%s:%s", r.app, sessionID)
}

func (r *RedisTaskBoardStore) Set(ctx context.Context, sessionID string, tasks []TaskItem) error {
	data, err := json.Marshal(tasks)
	if err != nil {
		return err
	}
	secs := int64(r.ttl.Seconds())
	return r.client.Do(ctx,
		r.client.B().Set().Key(r.key(sessionID)).Value(string(data)).ExSeconds(secs).Build(),
	).Error()
}

func (r *RedisTaskBoardStore) Get(ctx context.Context, sessionID string) ([]TaskItem, error) {
	s, err := r.client.Do(ctx, r.client.B().Get().Key(r.key(sessionID)).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, err
	}
	var tasks []TaskItem
	if err := json.Unmarshal([]byte(s), &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}
