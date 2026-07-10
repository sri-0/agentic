package eventlog

import (
	"context"
	"testing"
	"time"
)

// TestMemoryLog_HeartbeatOnIdleFollow verifies an idle follow reader receives a
// heartbeat SeqEvent (Seq == -1) so proxies don't idle-kill the SSE stream.
func TestMemoryLog_HeartbeatOnIdleFollow(t *testing.T) {
	orig := memHeartbeatInterval
	memHeartbeatInterval = 20 * time.Millisecond
	defer func() { memHeartbeatInterval = orig }()

	m := NewMemoryLog()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	ch, err := m.Read(ctx, "s1", 0, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case se, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before heartbeat")
		}
		if se.Seq != -1 || se.Event.Type != EvHeartbeat {
			t.Fatalf("first idle event = %+v, want heartbeat Seq=-1", se)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("no heartbeat within 300ms on idle follow reader")
	}
}

// TestMemoryLog_HeartbeatInterleavesWithRealEvents verifies real appends are
// still delivered gap-free while heartbeats tick.
func TestMemoryLog_HeartbeatInterleavesWithRealEvents(t *testing.T) {
	orig := memHeartbeatInterval
	memHeartbeatInterval = 15 * time.Millisecond
	defer func() { memHeartbeatInterval = orig }()

	m := NewMemoryLog()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch, _ := m.Read(ctx, "s2", 0, true)
	go func() {
		time.Sleep(40 * time.Millisecond)
		_, _ = m.Append(context.Background(), "s2", AgentEvent{Type: EvTextDelta, Text: "hi", IsOutput: true})
	}()

	var gotReal bool
	deadline := time.After(400 * time.Millisecond)
	for !gotReal {
		select {
		case se, ok := <-ch:
			if !ok {
				t.Fatal("channel closed early")
			}
			if se.Seq == -1 {
				continue // heartbeat
			}
			if se.Seq != 1 || se.Event.Text != "hi" {
				t.Fatalf("real event = %+v, want seq 1 text hi", se)
			}
			gotReal = true
		case <-deadline:
			t.Fatal("real event not delivered amid heartbeats")
		}
	}
}
