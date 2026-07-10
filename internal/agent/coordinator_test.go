package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentic/internal/eventlog"

	"github.com/rs/zerolog"
)

// drainEvents reads a session log from afterSeq (non-follow) and returns events.
func drainEvents(t *testing.T, log eventlog.EventLog, sessionID string, afterSeq int64) []eventlog.AgentEvent {
	t.Helper()
	ch, err := log.Read(context.Background(), sessionID, afterSeq, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var out []eventlog.AgentEvent
	timeout := time.After(2 * time.Second)
	for {
		select {
		case se, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, se.Event)
		case <-timeout:
			t.Fatal("timed out draining events")
		}
	}
}

// waitFor polls cond until true or the deadline; fails otherwise.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// W0/C1 reproduction (coordinator level): two sequential turns on one session.
// A run-attach reader created for turn 2 (after = startSeq-1) must receive
// turn-2 events and NOT re-close on turn-1's terminal.
func TestCoordinator_MultiTurn_SecondTurnStreams(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())

	// Turn 1: emit one text delta then finish.
	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		enc.Text("turn1")
		return runOutcome{status: RunDone}
	}
	h1, err := c.Start(RunRequest{SessionID: "s", UserID: "u"})
	if err != nil {
		t.Fatalf("Start turn1: %v", err)
	}
	waitFor(t, "turn1 done", func() bool {
		hh, _ := c.Status("u", "s")
		return hh != nil && hh.Status == RunDone
	})
	_ = h1

	head1, _ := log.Head(context.Background(), "s")

	// Turn 2: attach a run-attach reader BEFORE turn 2 emits (after = startSeq-1).
	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		time.Sleep(20 * time.Millisecond)
		enc.Text("turn2")
		return runOutcome{status: RunDone}
	}
	h2, err := c.Start(RunRequest{SessionID: "s", UserID: "u"})
	if err != nil {
		t.Fatalf("Start turn2: %v", err)
	}
	if h2.StartSeq <= head1 {
		t.Fatalf("turn2 StartSeq=%d should be > head1=%d", h2.StartSeq, head1)
	}

	// Run-attach reader from after = StartSeq-1.
	ch, _ := log.Read(context.Background(), "s", h2.StartSeq-1, true)
	var texts []string
	timeout := time.After(2 * time.Second)
loop:
	for {
		select {
		case se, ok := <-ch:
			if !ok {
				break loop
			}
			if se.Event.Type == eventlog.EvTextDelta {
				texts = append(texts, se.Event.Text)
			}
			if se.Event.IsTerminal() {
				break loop
			}
		case <-timeout:
			t.Fatal("timed out reading turn2")
		}
	}

	if len(texts) != 1 || texts[0] != "turn2" {
		t.Fatalf("run-attach reader saw %v, want [turn2] (must not re-close on turn1 terminal)", texts)
	}
}

// W0/C2 reproduction: a concurrent turn while running must NOT be silently
// dropped. The second Start enqueues; both turns execute and both messages land
// in the log.
func TestCoordinator_ConcurrentTurn_Queued(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())

	var mu sync.Mutex
	var executed []string
	release := make(chan struct{})

	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		mu.Lock()
		executed = append(executed, req.turnKey)
		mu.Unlock()
		if req.turnKey == "turn1" {
			<-release // hold turn1 open so turn2 arrives while running
		}
		enc.Text(req.turnKey)
		return runOutcome{status: RunDone}
	}

	h1, err := c.Start(RunRequest{SessionID: "s", UserID: "u", turnKey: "turn1"})
	if err != nil {
		t.Fatalf("Start turn1: %v", err)
	}
	waitFor(t, "turn1 running", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(executed) == 1
	})

	// Second turn while turn1 is still running: must be queued, not dropped.
	h2, err := c.Start(RunRequest{SessionID: "s", UserID: "u", turnKey: "turn2"})
	if err != nil {
		t.Fatalf("Start turn2: %v", err)
	}
	if h2 == nil {
		t.Fatal("turn2 handle nil")
	}
	_ = h1

	close(release) // let turn1 finish; turn2 should then drain

	waitFor(t, "both turns executed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(executed) == 2
	})

	mu.Lock()
	defer mu.Unlock()
	if executed[0] != "turn1" || executed[1] != "turn2" {
		t.Fatalf("execution order/content wrong: %v", executed)
	}
}

// W0/H5 reproduction: a run whose execution errors ends with StatusError, not
// StatusDone.
func TestCoordinator_ErrorRun_StatusError(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())

	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		return runOutcome{status: RunError, err: "boom"}
	}
	_, err := c.Start(RunRequest{SessionID: "s", UserID: "u"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "run finished", func() bool {
		hh, _ := c.Status("u", "s")
		return hh != nil && (hh.Status == RunError || hh.Status == RunDone)
	})

	h, _ := c.Status("u", "s")
	if h.Status != RunError {
		t.Fatalf("status=%s want error", h.Status)
	}

	evs := drainEvents(t, log, "s", 0)
	var lastTerminal *eventlog.AgentEvent
	for i := range evs {
		if evs[i].IsTerminal() {
			lastTerminal = &evs[i]
		}
	}
	if lastTerminal == nil || lastTerminal.Status != eventlog.StatusError {
		t.Fatalf("last terminal event = %+v, want status=error", lastTerminal)
	}
}

// W0/H5 reproduction: Cancel's terminal must win; the run goroutine's natural
// finish must not append a SECOND terminal that flips the status back.
func TestCoordinator_Cancel_NoDoubleTerminal(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())

	started := make(chan struct{})
	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		close(started)
		<-ctx.Done() // block until cancelled
		return runOutcome{status: RunDone}
	}
	_, err := c.Start(RunRequest{SessionID: "s", UserID: "u"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started
	if !c.Cancel("s") {
		t.Fatal("Cancel returned false")
	}

	waitFor(t, "cancelled", func() bool {
		hh, _ := c.Status("u", "s")
		return hh != nil && hh.Status == RunCancelled
	})

	// Give the run goroutine time to (wrongly) append a second terminal.
	time.Sleep(50 * time.Millisecond)

	evs := drainEvents(t, log, "s", 0)
	terminals := 0
	for _, e := range evs {
		if e.IsTerminal() {
			terminals++
			if e.Status != eventlog.StatusCancelled {
				t.Fatalf("terminal status=%s, want cancelled (cancel must win)", e.Status)
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("got %d terminal events, want exactly 1", terminals)
	}
}
