package eventlog

import (
	"context"
	"sync"
	"time"
)

const memSubBuffer = 512

// memHeartbeatInterval is how often an idle follow reader emits a heartbeat
// SeqEvent so proxies don't idle-kill a memory-backed SSE stream. The Redis
// backend already heartbeats on its XREAD BLOCK timeout; this gives the
// in-memory log the same keep-alive behaviour. It is a var (not const) so tests
// can shorten it; production keeps ~15s.
var memHeartbeatInterval = 15 * time.Second

// MemoryLog is an in-process EventLog. It retains the full durable backlog per
// session (the source of truth) and fans live events out to subscribers
// best-effort: if a subscriber's buffer is full the live copy is dropped, but it
// is never lost — a reader reconnects and replays from the backlog. This mirrors
// the codebase's in-memory session store and needs no external services.
//
// Terminal state belongs to RUNS, not sessions (see W1 remediation): a session
// log may hold many runs (multi-turn), so a terminal run-status NEVER closes a
// live follow subscriber. Follow reads close only when their ctx is cancelled.
// Non-follow reads close after draining the backlog. Closure policy for HTTP
// streams lives in the pump, not here.
type MemoryLog struct {
	mu       sync.Mutex
	sessions map[string]*memSession
}

type memSession struct {
	mu        sync.Mutex
	events    []AgentEvent // index i holds seq i+1
	subs      map[int]chan SeqEvent
	nextSub   int
	lastTouch time.Time // last Append/Read; for idle eviction
}

// NewMemoryLog returns an in-memory EventLog.
func NewMemoryLog() *MemoryLog {
	return &MemoryLog{sessions: make(map[string]*memSession)}
}

func (m *MemoryLog) session(id string) *memSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		s = &memSession{subs: make(map[int]chan SeqEvent), lastTouch: time.Now()}
		m.sessions[id] = s
	}
	return s
}

// Append assigns the next seq, stores ev, and fans it out to live subscribers.
func (m *MemoryLog) Append(_ context.Context, sessionID string, ev AgentEvent) (int64, error) {
	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMilli() // stamp at write so projections have a stable per-event time
	}
	s := m.session(sessionID)
	s.mu.Lock()
	s.events = append(s.events, ev)
	seq := int64(len(s.events))
	s.lastTouch = time.Now()
	se := SeqEvent{Seq: seq, Event: ev}
	subs := make([]chan SeqEvent, 0, len(s.subs))
	for _, ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- se:
		default: // live drop; durable backlog retains it for replay
		}
	}
	return seq, nil
}

// Read replays backlog after afterSeq, then (if follow) streams live events.
// Terminal events do NOT close a follow reader (runs own terminal state, not
// sessions); a follow reader closes only when ctx is cancelled. A negative
// afterSeq is clamped to 0.
func (m *MemoryLog) Read(ctx context.Context, sessionID string, afterSeq int64, follow bool) (<-chan SeqEvent, error) {
	if afterSeq < 0 {
		afterSeq = 0
	}
	s := m.session(sessionID)

	// Snapshot the backlog AND register the subscriber under a single lock hold,
	// so no live append can slip between the two. The deferred unlock guarantees
	// an out-of-range slice (or any panic) never leaves the mutex held — the H1
	// poisoning bug. afterSeq is already clamped >= 0 above.
	var (
		backlog []AgentEvent
		sub     chan SeqEvent
		subID   int
	)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastTouch = time.Now()
		headAtRegister := int64(len(s.events))
		backlog = make([]AgentEvent, 0)
		if afterSeq < headAtRegister {
			backlog = append(backlog, s.events[afterSeq:]...)
		}
		if follow {
			sub = make(chan SeqEvent, memSubBuffer)
			subID = s.nextSub
			s.nextSub++
			s.subs[subID] = sub
		}
	}()

	out := make(chan SeqEvent)
	go func() {
		defer close(out)
		defer func() {
			if sub != nil {
				s.mu.Lock()
				delete(s.subs, subID)
				s.mu.Unlock()
			}
		}()

		// lastSeq is the highest seq delivered to out; delivery is gap-free.
		lastSeq := afterSeq

		// Replay backlog: seq = afterSeq + 1 + i.
		for i, ev := range backlog {
			seq := afterSeq + 1 + int64(i)
			if !send(ctx, out, SeqEvent{Seq: seq, Event: ev}) {
				return
			}
			lastSeq = seq
		}
		if !follow || sub == nil {
			return
		}
		// Live tail: sub carries events with seq > the head at register time.
		// Live copies may be dropped under load, so before delivering any live
		// event we fill any gap [lastSeq+1, se.Seq) from the durable backlog —
		// guaranteeing exactly-once, gap-free delivery to out. Terminal events do
		// NOT close the stream; only ctx cancellation does. An idle ticker emits a
		// transient heartbeat (Seq == -1) so the SSE stream isn't idle-killed by a
		// proxy — mirrors the Redis backend's XREAD-timeout heartbeat.
		hb := time.NewTicker(memHeartbeatInterval)
		defer hb.Stop()
		for {
			select {
			case se, ok := <-sub:
				if !ok {
					return
				}
				if se.Seq <= lastSeq {
					continue // already delivered
				}
				if se.Seq > lastSeq+1 {
					if !replayRange(ctx, s, lastSeq, se.Seq-1, &lastSeq, out) {
						return
					}
				}
				if !send(ctx, out, se) {
					return
				}
				lastSeq = se.Seq
			case <-hb.C:
				if !send(ctx, out, SeqEvent{Seq: -1, Event: AgentEvent{V: 1, Type: EvHeartbeat}}) {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func send(ctx context.Context, out chan<- SeqEvent, se SeqEvent) bool {
	select {
	case out <- se:
		return true
	case <-ctx.Done():
		return false
	}
}

// replayRange delivers durable events with seq in [from+1, to], updating last.
func replayRange(ctx context.Context, s *memSession, from, to int64, last *int64, out chan<- SeqEvent) bool {
	s.mu.Lock()
	evs := append([]AgentEvent(nil), s.events...)
	s.mu.Unlock()
	for seq := from + 1; seq <= to && seq <= int64(len(evs)); seq++ {
		if !send(ctx, out, SeqEvent{Seq: seq, Event: evs[seq-1]}) {
			return false
		}
		*last = seq
	}
	return true
}

// Head returns the latest seq for sessionID.
func (m *MemoryLog) Head(_ context.Context, sessionID string) (int64, error) {
	s := m.session(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.events)), nil
}

// Evict removes a session's log and subscribers. Safe to call for an unknown
// session (no-op). Used by the coordinator's idle sweeper (M1) to bound growth;
// active runs must not be evicted (the coordinator guards that).
func (m *MemoryLog) Evict(sessionID string) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	for id, ch := range s.subs {
		close(ch)
		delete(s.subs, id)
	}
	s.events = nil
	s.mu.Unlock()
}

// IdleSince reports whether the session exists and its last activity is older
// than the cutoff. Unknown sessions report (false).
func (m *MemoryLog) IdleSince(sessionID string, cutoff time.Time) bool {
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTouch.Before(cutoff)
}

// Close is a no-op for the in-memory log.
func (m *MemoryLog) Close() error { return nil }
