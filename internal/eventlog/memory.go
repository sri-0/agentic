package eventlog

import (
	"context"
	"sync"
)

const memSubBuffer = 512

// MemoryLog is an in-process EventLog. It retains the full durable backlog per
// session (the source of truth) and fans live events out to subscribers
// best-effort: if a subscriber's buffer is full the live copy is dropped, but it
// is never lost — a reader reconnects and replays from the backlog. This mirrors
// the codebase's in-memory session store and needs no external services.
type MemoryLog struct {
	mu       sync.Mutex
	sessions map[string]*memSession
}

type memSession struct {
	mu       sync.Mutex
	events   []AgentEvent // index i holds seq i+1
	terminal bool
	subs     map[int]chan SeqEvent
	nextSub  int
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
		s = &memSession{subs: make(map[int]chan SeqEvent)}
		m.sessions[id] = s
	}
	return s
}

// Append assigns the next seq, stores ev, and fans it out to live subscribers.
func (m *MemoryLog) Append(_ context.Context, sessionID string, ev AgentEvent) (int64, error) {
	s := m.session(sessionID)
	s.mu.Lock()
	s.events = append(s.events, ev)
	seq := int64(len(s.events))
	if ev.IsTerminal() {
		s.terminal = true
	}
	se := SeqEvent{Seq: seq, Event: ev}
	subs := make([]chan SeqEvent, 0, len(s.subs))
	for _, ch := range s.subs {
		subs = append(subs, ch)
	}
	terminal := ev.IsTerminal()
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- se:
		default: // live drop; durable backlog retains it for replay
		}
	}
	if terminal {
		// Wake live subscribers so they observe the terminal event in backlog
		// even if the live copy was dropped; they re-check via the channel close.
		s.mu.Lock()
		for id, ch := range s.subs {
			close(ch)
			delete(s.subs, id)
		}
		s.mu.Unlock()
	}
	return seq, nil
}

// Read replays backlog after afterSeq, then (if follow) streams live events.
func (m *MemoryLog) Read(ctx context.Context, sessionID string, afterSeq int64, follow bool) (<-chan SeqEvent, error) {
	s := m.session(sessionID)

	s.mu.Lock()
	headAtRegister := int64(len(s.events))
	backlog := make([]AgentEvent, 0)
	if afterSeq < headAtRegister {
		backlog = append(backlog, s.events[afterSeq:]...)
	}
	terminalAtRegister := s.terminal
	var sub chan SeqEvent
	var subID int
	if follow && !terminalAtRegister {
		sub = make(chan SeqEvent, memSubBuffer)
		subID = s.nextSub
		s.nextSub++
		s.subs[subID] = sub
	}
	s.mu.Unlock()

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
		if !follow || terminalAtRegister || sub == nil {
			return
		}
		// Live tail: sub carries events with seq > headAtRegister. Live copies
		// may be dropped under load, so before delivering any live event we fill
		// any gap [lastSeq+1, se.Seq) from the durable backlog — guaranteeing
		// exactly-once, gap-free delivery to out.
		for {
			select {
			case se, ok := <-sub:
				if !ok {
					// Terminal: flush anything appended after lastSeq.
					if !drainFrom(ctx, s, &lastSeq, out) {
						return
					}
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
				if se.Event.IsTerminal() {
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

// drainFrom delivers all durable events with seq > *last, updating *last.
func drainFrom(ctx context.Context, s *memSession, last *int64, out chan<- SeqEvent) bool {
	s.mu.Lock()
	head := int64(len(s.events))
	s.mu.Unlock()
	return replayRange(ctx, s, *last, head, last, out)
}

// Head returns the latest seq for sessionID.
func (m *MemoryLog) Head(_ context.Context, sessionID string) (int64, error) {
	s := m.session(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.events)), nil
}

// Close is a no-op for the in-memory log.
func (m *MemoryLog) Close() error { return nil }
