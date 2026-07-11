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

// Task A: a finished (done) session older than the retention window is evicted by
// sweep; a still-awaiting-input (paused HITL) session is NEVER evicted regardless
// of age. Uses a controllable clock via c.now and a MemoryLog (implements
// IdleSince/Evict).
func TestCoordinator_Sweep_RetentionAndHITLGuard(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())
	c.StopSweeper() // drive sweep manually
	c.SetSessionRetention(10 * time.Second)

	base := time.Now()
	c.now = func() time.Time { return base }

	// A done session.
	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		return runOutcome{status: RunDone}
	}
	if _, err := c.Start(RunRequest{SessionID: "done1", UserID: "u"}); err != nil {
		t.Fatalf("Start done1: %v", err)
	}
	waitFor(t, "done1 finished", func() bool {
		hh, _ := c.Status("u", "done1")
		return hh != nil && hh.Status == RunDone
	})

	// A paused HITL session: force the handle into awaiting-input directly (the
	// run seam returning awaiting-input routes through terminate → status set).
	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		return runOutcome{status: RunAwaitingInput}
	}
	if _, err := c.Start(RunRequest{SessionID: "paused1", UserID: "u"}); err != nil {
		t.Fatalf("Start paused1: %v", err)
	}
	waitFor(t, "paused1 awaiting", func() bool {
		hh, _ := c.Status("u", "paused1")
		return hh != nil && hh.Status == RunAwaitingInput
	})

	// Advance the clock beyond retention and sweep.
	c.now = func() time.Time { return base.Add(30 * time.Second) }
	c.sweep()

	if _, ok := c.Status("u", "done1"); ok {
		t.Fatal("done1 should have been evicted after retention")
	}
	if _, ok := c.Status("u", "paused1"); !ok {
		t.Fatal("paused1 (awaiting-input) must NOT be evicted while paused")
	}
}

// Task B: on a hard terminal the session starts UNVIEWED; MarkViewed flips it;
// cross-user MarkViewed is rejected (ownership).
func TestCoordinator_ViewedFlow(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())
	c.StopSweeper()
	c.SetViewedStore(NewMemoryViewedStore())

	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		return runOutcome{status: RunDone}
	}
	if _, err := c.Start(RunRequest{SessionID: "s", UserID: "owner"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "done", func() bool {
		hh, _ := c.Status("owner", "s")
		return hh != nil && hh.Status == RunDone
	})

	h, _ := c.Status("owner", "s")
	if h.Viewed {
		t.Fatal("session should start UNVIEWED after terminal")
	}
	// Cross-user MarkViewed rejected.
	if c.MarkViewed("intruder", "s") {
		t.Fatal("cross-user MarkViewed must be rejected")
	}
	// Owner MarkViewed succeeds and flips the flag.
	if !c.MarkViewed("owner", "s") {
		t.Fatal("owner MarkViewed should succeed")
	}
	h2, _ := c.Status("owner", "s")
	if !h2.Viewed {
		t.Fatal("session should be viewed=true after MarkViewed")
	}
}

// Task C: the terminal archive flush is AWAITED before the terminal run-status
// event is appended, so no reader can observe `done` before the flush completes.
func TestCoordinator_TerminalFlush_BeforeTerminalEvent(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())
	c.StopSweeper()

	var flushed bool
	var flushedBeforeTerminal bool
	c.SetTerminalFlusher(TerminalFlusherFunc(func(ctx context.Context, app, userID, sessionID string) error {
		// At flush time the terminal event must NOT yet be in the log.
		evs := drainEvents(t, log, sessionID, 0)
		hasTerminal := false
		for _, e := range evs {
			if e.IsTerminal() {
				hasTerminal = true
			}
		}
		flushed = true
		flushedBeforeTerminal = !hasTerminal
		return nil
	}), "app")

	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		return runOutcome{status: RunDone}
	}
	if _, err := c.Start(RunRequest{SessionID: "s", UserID: "u"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "done", func() bool {
		hh, _ := c.Status("u", "s")
		return hh != nil && hh.Status == RunDone
	})
	if !flushed {
		t.Fatal("terminal flusher was not invoked")
	}
	if !flushedBeforeTerminal {
		t.Fatal("terminal flush must run BEFORE the terminal run-status event is appended")
	}
}
