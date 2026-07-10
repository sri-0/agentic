package eventlog

import (
	"context"
	"testing"
	"time"
)

// W0/C1 reproduction: after a terminal event, a NEW follow reader must still
// live-deliver subsequently-appended events. This is the sticky-terminal bug:
// runs (not sessions) own terminal state, so a second turn appended to the same
// session must reach a reader that attached after the first turn's terminal.
func TestMemoryLog_TerminalDoesNotStickyCloseNewFollow(t *testing.T) {
	l := NewMemoryLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Turn 1: ends in a terminal done.
	l.Append(ctx, "s", textEv("turn1-a"))
	l.Append(ctx, "s", doneEv())

	head, _ := l.Head(ctx, "s")
	if head != 2 {
		t.Fatalf("head=%d want 2", head)
	}

	// Turn 2 attaches AFTER turn 1 (run-attach: after = startSeq-1 = head).
	ch, _ := l.Read(ctx, "s", head, true)

	// Turn 2 events appended after the reader registered.
	go func() {
		time.Sleep(20 * time.Millisecond)
		l.Append(ctx, "s", textEv("turn2-a"))
		l.Append(ctx, "s", doneEv())
	}()

	got := collectN(t, ch, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 turn-2 events, got %d: %+v", len(got), got)
	}
	if got[0].Event.Text != "turn2-a" {
		t.Fatalf("first turn-2 event wrong: %+v", got[0].Event)
	}
	if got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("turn-2 seqs wrong: %d, %d (want 3, 4)", got[0].Seq, got[1].Seq)
	}
}

// W0/H1 reproduction: Read with a negative afterSeq must not panic and the
// session must remain usable (a subsequent Append/Head succeeds — the mutex is
// not poisoned).
func TestMemoryLog_NegativeAfterSeqNoPanic(t *testing.T) {
	l := NewMemoryLog()
	ctx := context.Background()
	l.Append(ctx, "s", textEv("a"))

	ch, err := l.Read(ctx, "s", -1, false)
	if err != nil {
		t.Fatalf("Read(-1) error: %v", err)
	}
	// Drain; must not panic.
	collect(t, ch)

	// Session must still be usable.
	if _, err := l.Append(ctx, "s", textEv("b")); err != nil {
		t.Fatalf("Append after negative Read failed: %v", err)
	}
	h, err := l.Head(ctx, "s")
	if err != nil {
		t.Fatalf("Head after negative Read failed: %v", err)
	}
	if h != 2 {
		t.Fatalf("head=%d want 2", h)
	}
}

// collectN reads up to n events (or until channel close / timeout).
func collectN(t *testing.T, ch <-chan SeqEvent, n int) []SeqEvent {
	t.Helper()
	var out []SeqEvent
	timeout := time.After(2 * time.Second)
	for len(out) < n {
		select {
		case se, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, se)
		case <-timeout:
			return out
		}
	}
	return out
}
