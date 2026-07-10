package agent

import (
	"context"
	"sync"
	"testing"

	"agentic/internal/eventlog"
	"agentic/internal/stream"

	"github.com/rs/zerolog"
)

// TestPostRunHook_FiresOnTerminal verifies a registered hook fires exactly once
// with the run's terminal status on a normal finish.
func TestPostRunHook_FiresOnTerminal(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())
	defer c.StopSweeper()

	var mu sync.Mutex
	var got []PostRunInfo
	c.AddPostRunHook(func(info PostRunInfo) {
		mu.Lock()
		got = append(got, info)
		mu.Unlock()
	})

	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		enc.Text("hi")
		return runOutcome{status: RunDone}
	}
	if _, err := c.Start(RunRequest{SessionID: "s", UserID: "u"}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "hook fired", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if got[0].Status != RunDone || got[0].SessionID != "s" || got[0].UserID != "u" {
		t.Fatalf("hook info = %+v", got[0])
	}
}

// TestPostRunHook_NotFiredOnAwaitingInput verifies awaiting-input (a suspend, not
// a terminal) does NOT fire post-run hooks.
func TestPostRunHook_NotFiredOnAwaitingInput(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())
	defer c.StopSweeper()

	var count int
	var mu sync.Mutex
	c.AddPostRunHook(func(PostRunInfo) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		return runOutcome{status: RunAwaitingInput}
	}
	if _, err := c.Start(RunRequest{SessionID: "s", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "run suspended", func() bool {
		hh, _ := c.Status("u", "s")
		return hh != nil && hh.Status == RunAwaitingInput
	})
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("hook fired %d times on awaiting-input, want 0", count)
	}
}

// TestPostRunHook_FiresOnceOnCancel verifies Cancel (which routes through the
// same once-guarded terminate) fires the hook exactly once with Cancelled.
func TestPostRunHook_FiresOnceOnCancel(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())
	defer c.StopSweeper()

	var mu sync.Mutex
	var statuses []RunStatus
	c.AddPostRunHook(func(info PostRunInfo) {
		mu.Lock()
		statuses = append(statuses, info.Status)
		mu.Unlock()
	})

	block := make(chan struct{})
	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		<-block // hold the run open until cancelled
		return runOutcome{status: RunDone}
	}
	if _, err := c.Start(RunRequest{SessionID: "s", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "run active", func() bool {
		hh, _ := c.Status("u", "s")
		return hh != nil && hh.Status == RunRunning
	})
	c.Cancel("s")
	close(block) // let the run goroutine finish; its finish() must be a no-op

	waitFor(t, "hook fired once", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(statuses) >= 1
	})
	// give any erroneous second fire a chance.
	waitFor(t, "settled", func() bool {
		hh, _ := c.Status("u", "s")
		return hh != nil && hh.Status == RunCancelled
	})
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 1 || statuses[0] != RunCancelled {
		t.Fatalf("post-run statuses = %v, want exactly [cancelled]", statuses)
	}
}

// TestTaskBoardStore_CapturedByEncoder verifies the encoder writes task-list
// snapshots to the wired board store (Task 4).
func TestTaskBoardStore_CapturedByEncoder(t *testing.T) {
	log := eventlog.NewMemoryLog()
	c := NewCoordinator(log, zerolog.Nop())
	defer c.StopSweeper()
	store := eventlog.NewMemoryTaskBoardStore()
	c.SetTaskBoardStore(store)

	c.runFn = func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
		enc.TaskList([]stream.Task{{ID: "t1", Title: "Do it", Status: "pending"}})
		return runOutcome{status: RunDone}
	}
	if _, err := c.Start(RunRequest{SessionID: "s", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "board written", func() bool {
		b := c.TaskBoard(context.Background(), "s")
		return len(b) == 1 && b[0].ID == "t1"
	})
}
