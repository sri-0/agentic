package agent

import (
	"context"

	"agentic/internal/config"
	"agentic/internal/eventlog"
	pkgvalkey "agentic/pkg/db/valkey"

	"github.com/rs/zerolog"
)

// NewEventLog creates an eventlog.EventLog based on cfg.EventLogStore, mirroring
// NewSessionService. Set EVENTLOG_STORE=redis to use Redis Streams (requires
// Valkey config); defaults to the in-memory log.
func NewEventLog(cfg *config.Config, logger zerolog.Logger) (eventlog.EventLog, error) {
	switch cfg.EventLogStore {
	case "redis", "valkey":
		if cfg.Valkey == nil {
			logger.Warn().Msg("eventlog: EVENTLOG_STORE=redis but no Valkey config; falling back to in-memory")
			return eventlog.NewMemoryLog(), nil
		}
		v, err := pkgvalkey.New(context.Background(), *cfg.Valkey)
		if err != nil {
			logger.Warn().Err(err).Msg("eventlog: valkey unavailable; falling back to in-memory")
			return eventlog.NewMemoryLog(), nil
		}
		logger.Info().Msg("eventlog: using redis streams store")
		return eventlog.NewRedisStreamLog(v.Client, cfg.AppName), nil
	default:
		logger.Info().Msg("eventlog: using in-memory store")
		return eventlog.NewMemoryLog(), nil
	}
}
