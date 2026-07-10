package eventlog

import "context"

// SeqEvent is an AgentEvent stamped with its per-session monotonic sequence.
// Seq == -1 marks a transient heartbeat (not part of the durable sequence).
type SeqEvent struct {
	Seq   int64
	Event AgentEvent
}

// EventLog is the durable, append-only, per-session event store. Implementations
// must guarantee per-session monotonic, gap-free sequence numbers and the
// "replay-then-live" contract on Read.
//
// Single-writer per session is assumed (the run coordinator goroutine owns
// Append for a session); readers are unconstrained and may be many.
type EventLog interface {
	// Append assigns the next seq for sessionID, durably stores ev, and fans it
	// out to live readers. Returns the assigned seq.
	Append(ctx context.Context, sessionID string, ev AgentEvent) (seq int64, err error)

	// Read returns a channel delivering events with seq > afterSeq in order.
	// It first replays the durable backlog, then (if follow) continues with live
	// events until ctx is cancelled or a terminal run-status is delivered, at
	// which point the channel is closed. With follow=false it closes after the
	// backlog. afterSeq=0 starts from the beginning.
	Read(ctx context.Context, sessionID string, afterSeq int64, follow bool) (<-chan SeqEvent, error)

	// Head returns the latest assigned seq for sessionID (0 if none).
	Head(ctx context.Context, sessionID string) (int64, error)

	// Close releases resources.
	Close() error
}
