package agent

import (
	"context"
	"sync"
	"time"

	"agentic/internal/config"
	pkgvalkey "agentic/pkg/db/valkey"

	"github.com/rs/zerolog"
	valkey "github.com/valkey-io/valkey-go"
)

// ViewedStore tracks a per-session "viewed" flag for the owning user (Task B).
// On a run terminal the owner has NOT yet seen the result, so the session starts
// UNVIEWED; the frontend renders done + !viewed as the completed-but-unseen ring.
// POST /v1/sessions/{id}/viewed marks it viewed.
//
// Two implementations mirror the TaskBoardStore pattern: a Redis/Valkey-backed
// store (key viewed:{app}:{session}, TTL aligned to SESSION_RETENTION so it self-
// cleans with the session) and an in-memory fallback used when Valkey isn't
// configured. All methods are cheap and degrade safely.
type ViewedStore interface {
	// SetUnviewed records that the session finished and has not been viewed. ttl
	// bounds how long the flag persists (aligned to session retention).
	SetUnviewed(ctx context.Context, sessionID string, ttl time.Duration) error
	// MarkViewed marks the session as viewed by its owner.
	MarkViewed(ctx context.Context, sessionID string) error
	// Viewed reports whether the session has been viewed. A session with no
	// recorded terminal (never finished, or already swept) reports true — viewed
	// is only meaningful for a finished session, and "unseen" is the exception.
	Viewed(ctx context.Context, sessionID string) (bool, error)
}

// MemoryViewedStore is the in-memory fallback ViewedStore.
type MemoryViewedStore struct {
	mu     sync.RWMutex
	viewed map[string]bool // sessionID -> viewed (present only once a terminal is recorded)
}

// NewMemoryViewedStore returns an in-memory viewed store.
func NewMemoryViewedStore() *MemoryViewedStore {
	return &MemoryViewedStore{viewed: map[string]bool{}}
}

func (m *MemoryViewedStore) SetUnviewed(_ context.Context, sessionID string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.viewed[sessionID] = false
	return nil
}

func (m *MemoryViewedStore) MarkViewed(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.viewed[sessionID] = true
	return nil
}

func (m *MemoryViewedStore) Viewed(_ context.Context, sessionID string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.viewed[sessionID]
	if !ok {
		return true, nil // no recorded terminal → treat as viewed (not "unseen")
	}
	return v, nil
}

// RedisViewedStore persists the flag to viewed:{app}:{session}. The value is "0"
// (unviewed) or "1" (viewed); absence means no terminal recorded → viewed.
type RedisViewedStore struct {
	client valkey.Client
	app    string
}

// NewRedisViewedStore wraps a valkey client as a ViewedStore.
func NewRedisViewedStore(client valkey.Client, app string) *RedisViewedStore {
	return &RedisViewedStore{client: client, app: app}
}

func (r *RedisViewedStore) key(sessionID string) string {
	return "viewed:" + r.app + ":" + sessionID
}

func (r *RedisViewedStore) SetUnviewed(ctx context.Context, sessionID string, ttl time.Duration) error {
	secs := int64(ttl.Seconds())
	if secs <= 0 {
		secs = 1
	}
	return r.client.Do(ctx,
		r.client.B().Set().Key(r.key(sessionID)).Value("0").ExSeconds(secs).Build(),
	).Error()
}

func (r *RedisViewedStore) MarkViewed(ctx context.Context, sessionID string) error {
	// KEEPTTL: preserve the retention-aligned expiry set at SetUnviewed time.
	return r.client.Do(ctx,
		r.client.B().Set().Key(r.key(sessionID)).Value("1").Keepttl().Build(),
	).Error()
}

func (r *RedisViewedStore) Viewed(ctx context.Context, sessionID string) (bool, error) {
	s, err := r.client.Do(ctx, r.client.B().Get().Key(r.key(sessionID)).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return true, nil // no terminal recorded (or expired) → viewed
		}
		return true, err
	}
	return s == "1", nil
}

// NewViewedStore creates the per-session viewed store (Task B). It uses Redis
// when EVENTLOG_STORE=redis/valkey and Valkey is reachable (mirroring
// NewTaskBoardStore), and otherwise degrades to an in-memory store so the default
// path keeps working without external services.
func NewViewedStore(cfg *config.Config, logger zerolog.Logger) ViewedStore {
	switch cfg.EventLogStore {
	case "redis", "valkey":
		if cfg.Valkey == nil {
			logger.Warn().Msg("viewed: EVENTLOG_STORE=redis but no Valkey config; using in-memory")
			return NewMemoryViewedStore()
		}
		v, err := pkgvalkey.New(context.Background(), *cfg.Valkey)
		if err != nil {
			logger.Warn().Err(err).Msg("viewed: valkey unavailable; using in-memory")
			return NewMemoryViewedStore()
		}
		logger.Info().Msg("viewed: using redis store")
		return NewRedisViewedStore(v.Client, cfg.AppName)
	default:
		return NewMemoryViewedStore()
	}
}
