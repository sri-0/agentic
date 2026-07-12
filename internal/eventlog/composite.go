package eventlog

import (
	"context"
	"time"
)

// ColdStore is the durable cold archive of a session's events (e.g. OpenSearch
// session_events), read back after the hot Redis window (TTL) expires. It only
// serves history; live tailing is always hot-store (Redis/memory) only.
type ColdStore interface {
	// ReadHistory returns the full ordered event backlog for a session, or an
	// empty slice if none is archived.
	ReadHistory(ctx context.Context, sessionID string) ([]SeqEvent, error)
}

// CompositeLog serves live reads and recent history from a hot EventLog
// (Redis/memory) and falls back to a ColdStore for history when the hot store
// has expired (Head == 0). Appends and live follow reads go to the hot store
// only — the cold store is populated asynchronously by the archiver on terminal
// run status. This gives a resumable hot window backed by durable cold replay.
type CompositeLog struct {
	EventLog
	cold ColdStore
}

// NewCompositeLog wraps a hot EventLog with a cold-archive fallback.
func NewCompositeLog(hot EventLog, cold ColdStore) *CompositeLog {
	return &CompositeLog{EventLog: hot, cold: cold}
}

// Read serves from the hot store, but for a NON-follow read of a session the hot
// store no longer has (Head == 0, TTL expired) it replays the cold archive
// instead. Live follow reads are hot-only — a caller that wants to tail a live
// run reads the hot store directly. afterSeq is honoured against the cold
// backlog too (seq is stable across hot and cold).
func (c *CompositeLog) Read(ctx context.Context, sessionID string, afterSeq int64, follow bool) (<-chan SeqEvent, error) {
	if follow || c.cold == nil {
		return c.EventLog.Read(ctx, sessionID, afterSeq, follow)
	}
	head, err := c.EventLog.Head(ctx, sessionID)
	if err == nil && head > 0 {
		return c.EventLog.Read(ctx, sessionID, afterSeq, false)
	}
	// Hot store expired/empty — replay from cold.
	events, err := c.cold.ReadHistory(ctx, sessionID)
	if err != nil {
		// Fall back to the hot store's (empty) read rather than erroring, so a
		// missing/broken cold store degrades to "no history" not a failure.
		return c.EventLog.Read(ctx, sessionID, afterSeq, false)
	}
	if afterSeq < 0 {
		afterSeq = 0
	}
	out := make(chan SeqEvent)
	go func() {
		defer close(out)
		for _, se := range events {
			if se.Seq <= afterSeq {
				continue
			}
			select {
			case out <- se:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Evict delegates to the hot store's eviction (M1 idle sweep) when supported.
// The cold archive is durable and not swept here.
func (c *CompositeLog) Evict(sessionID string) {
	if ev, ok := c.EventLog.(interface{ Evict(string) }); ok {
		ev.Evict(sessionID)
	}
}

// IdleSince delegates to the hot store's idle check when supported; if the hot
// store doesn't track idleness the session is not considered evictable.
func (c *CompositeLog) IdleSince(sessionID string, cutoff time.Time) bool {
	if ev, ok := c.EventLog.(interface {
		IdleSince(string, time.Time) bool
	}); ok {
		return ev.IdleSince(sessionID, cutoff)
	}
	return false
}

// Head prefers the hot store; if expired (0) it reports the cold archive's head.
func (c *CompositeLog) Head(ctx context.Context, sessionID string) (int64, error) {
	head, err := c.EventLog.Head(ctx, sessionID)
	if err == nil && head > 0 {
		return head, nil
	}
	if c.cold == nil {
		return head, err
	}
	events, cerr := c.cold.ReadHistory(ctx, sessionID)
	if cerr != nil || len(events) == 0 {
		return head, err
	}
	return events[len(events)-1].Seq, nil
}
