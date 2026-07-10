package eventlog

import (
	"context"
	"testing"
	"time"
)

func textEv(s string) AgentEvent  { return AgentEvent{V: 1, Type: EvTextDelta, Text: s} }
func doneEv() AgentEvent          { return AgentEvent{V: 1, Type: EvRunStatus, Status: StatusDone} }

func collect(t *testing.T, ch <-chan SeqEvent) []SeqEvent {
	t.Helper()
	var out []SeqEvent
	timeout := time.After(2 * time.Second)
	for {
		select {
		case se, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, se)
		case <-timeout:
			t.Fatal("timed out waiting for events")
		}
	}
}

func TestMemoryLogSeqMonotonic(t *testing.T) {
	l := NewMemoryLog()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		seq, err := l.Append(ctx, "s1", textEv("x"))
		if err != nil {
			t.Fatal(err)
		}
		if seq != int64(i+1) {
			t.Fatalf("append %d: seq=%d want %d", i, seq, i+1)
		}
	}
	if h, _ := l.Head(ctx, "s1"); h != 5 {
		t.Fatalf("head=%d want 5", h)
	}
}

func TestMemoryLogBacklogReplayNonFollow(t *testing.T) {
	l := NewMemoryLog()
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c"} {
		l.Append(ctx, "s", textEv(s))
	}
	ch, _ := l.Read(ctx, "s", 0, false)
	got := collect(t, ch)
	if len(got) != 3 || got[0].Seq != 1 || got[2].Seq != 3 || got[2].Event.Text != "c" {
		t.Fatalf("backlog replay wrong: %+v", got)
	}
}

func TestMemoryLogResumeAfterSeq(t *testing.T) {
	l := NewMemoryLog()
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c", "d"} {
		l.Append(ctx, "s", textEv(s))
	}
	ch, _ := l.Read(ctx, "s", 2, false) // resume after seq 2
	got := collect(t, ch)
	if len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("resume wrong: %+v", got)
	}
}

func TestMemoryLogReplayThenLive(t *testing.T) {
	l := NewMemoryLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Append(ctx, "s", textEv("backlog1"))
	l.Append(ctx, "s", textEv("backlog2"))

	ch, _ := l.Read(ctx, "s", 0, true)
	// live appends after Read registered
	go func() {
		time.Sleep(20 * time.Millisecond)
		l.Append(ctx, "s", textEv("live3"))
		l.Append(ctx, "s", doneEv()) // terminal closes the stream
	}()
	got := collect(t, ch)
	if len(got) != 4 {
		t.Fatalf("want 4 events (2 backlog + live + done), got %d: %+v", len(got), got)
	}
	for i, se := range got {
		if se.Seq != int64(i+1) {
			t.Fatalf("event %d seq=%d want %d", i, se.Seq, i+1)
		}
	}
	if got[3].Event.Type != EvRunStatus {
		t.Fatalf("last event not terminal: %+v", got[3].Event)
	}
}

func TestMemoryLogTerminalBeforeFollowCloses(t *testing.T) {
	l := NewMemoryLog()
	ctx := context.Background()
	l.Append(ctx, "s", textEv("a"))
	l.Append(ctx, "s", doneEv())
	// follow=true but run already terminal: must replay backlog then close.
	ch, _ := l.Read(ctx, "s", 0, true)
	got := collect(t, ch)
	if len(got) != 2 || !got[1].Event.IsTerminal() {
		t.Fatalf("terminal-before-follow wrong: %+v", got)
	}
}
