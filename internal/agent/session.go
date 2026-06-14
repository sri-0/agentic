package agent

import (
	"context"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"google.golang.org/adk/session"
)

// SessionManager wraps the ADK session service with lazy creation.
type SessionManager struct {
	service session.Service
	appName string
	mu      sync.RWMutex
	known   map[string]bool // tracks which sessions have been created
	logger  zerolog.Logger
}

func NewSessionManager(service session.Service, appName string, logger zerolog.Logger) *SessionManager {
	return &SessionManager{
		service: service,
		appName: appName,
		known:   make(map[string]bool),
		logger:  logger,
	}
}

// GetOrCreate ensures a session exists for the given threadID, creating one if needed.
// userID defaults to "default" if empty.
func (m *SessionManager) GetOrCreate(ctx context.Context, threadID string, userID ...string) error {
	uid := "default"
	if len(userID) > 0 && userID[0] != "" {
		uid = userID[0]
	}

	key := uid + ":" + threadID
	m.mu.RLock()
	exists := m.known[key]
	m.mu.RUnlock()

	if exists {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if m.known[key] {
		return nil
	}

	// The known map is per-manager, but per-request model overrides rebuild the
	// manager (and its map) while sharing the underlying session store. Consult
	// the store directly so a follow-up turn reuses the existing session instead
	// of trying to recreate it.
	if _, err := m.service.Get(ctx, &session.GetRequest{
		AppName:   m.appName,
		UserID:    uid,
		SessionID: threadID,
	}); err == nil {
		m.known[key] = true
		return nil
	}

	_, err := m.service.Create(ctx, &session.CreateRequest{
		AppName:   m.appName,
		UserID:    uid,
		SessionID: threadID,
	})
	// Tolerate a race where the session appeared between Get and Create.
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}

	m.known[key] = true
	m.logger.Debug().Str("thread_id", threadID).Str("user_id", uid).Msg("session ready")
	return nil
}

// Service returns the underlying session service.
func (m *SessionManager) Service() session.Service {
	return m.service
}
