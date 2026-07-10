package eventlog

import (
	"context"
	"testing"
)

// fakeCold is a static ColdStore for testing composite fallback.
type fakeCold struct{ events []SeqEvent }

func (f fakeCold) ReadHistory(_ context.Context, _ string) ([]SeqEvent, error) {
	return f.events, nil
}

func drain(ch <-chan SeqEvent) []SeqEvent {
	var out []SeqEvent
	for se := range ch {
		out = append(out, se)
	}
	return out
}

func TestCompositeLog_HotServesWhenPresent(t *testing.T) {
	hot := NewMemoryLog()
	ctx := context.Background()
	_, _ = hot.Append(ctx, "s", AgentEvent{Type: EvTextDelta, Text: "hot", IsOutput: true})

	cold := fakeCold{events: []SeqEvent{{Seq: 1, Event: AgentEvent{Type: EvTextDelta, Text: "cold"}}}}
	c := NewCompositeLog(hot, cold)

	ch, err := c.Read(ctx, "s", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	got := drain(ch)
	if len(got) != 1 || got[0].Event.Text != "hot" {
		t.Fatalf("hot present should serve hot; got %+v", got)
	}
}

func TestCompositeLog_ColdFallbackWhenHotExpired(t *testing.T) {
	hot := NewMemoryLog() // empty -> Head == 0
	ctx := context.Background()
	cold := fakeCold{events: []SeqEvent{
		{Seq: 1, Event: AgentEvent{Type: EvTextDelta, Text: "a"}},
		{Seq: 2, Event: AgentEvent{Type: EvTextDelta, Text: "b"}},
	}}
	c := NewCompositeLog(hot, cold)

	ch, err := c.Read(ctx, "s", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	got := drain(ch)
	if len(got) != 2 || got[0].Event.Text != "a" || got[1].Event.Text != "b" {
		t.Fatalf("cold fallback failed; got %+v", got)
	}
}

func TestCompositeLog_ColdRespectsAfterSeq(t *testing.T) {
	hot := NewMemoryLog()
	ctx := context.Background()
	cold := fakeCold{events: []SeqEvent{
		{Seq: 1, Event: AgentEvent{Text: "a"}},
		{Seq: 2, Event: AgentEvent{Text: "b"}},
		{Seq: 3, Event: AgentEvent{Text: "c"}},
	}}
	c := NewCompositeLog(hot, cold)

	ch, _ := c.Read(ctx, "s", 1, false)
	got := drain(ch)
	if len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("afterSeq not honoured on cold; got %+v", got)
	}
}

func TestCompositeLog_FollowIsHotOnly(t *testing.T) {
	hot := NewMemoryLog()
	ctx, cancel := context.WithCancel(context.Background())
	cold := fakeCold{events: []SeqEvent{{Seq: 1, Event: AgentEvent{Text: "cold"}}}}
	c := NewCompositeLog(hot, cold)

	ch, err := c.Read(ctx, "s", 0, true) // follow -> hot only, no cold events
	if err != nil {
		t.Fatal(err)
	}
	_, _ = hot.Append(context.Background(), "s", AgentEvent{Text: "live"})
	se := <-ch
	if se.Event.Text != "live" {
		t.Fatalf("follow should be hot-only live; got %+v", se)
	}
	cancel()
}

func TestCompositeLog_HeadPrefersHotThenCold(t *testing.T) {
	hot := NewMemoryLog()
	ctx := context.Background()
	cold := fakeCold{events: []SeqEvent{{Seq: 7, Event: AgentEvent{Text: "x"}}}}
	c := NewCompositeLog(hot, cold)

	if h, _ := c.Head(ctx, "s"); h != 7 {
		t.Errorf("Head with empty hot should report cold head 7, got %d", h)
	}
	_, _ = hot.Append(ctx, "s", AgentEvent{Text: "y"})
	if h, _ := c.Head(ctx, "s"); h != 1 {
		t.Errorf("Head with hot present should report hot head 1, got %d", h)
	}
}

func TestMemoryTaskBoardStore(t *testing.T) {
	s := NewMemoryTaskBoardStore()
	ctx := context.Background()
	if got, _ := s.Get(ctx, "s"); len(got) != 0 {
		t.Fatalf("empty board should be empty, got %+v", got)
	}
	tasks := []TaskItem{{ID: "1", Title: "a", Status: "pending"}}
	if err := s.Set(ctx, "s", tasks); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "s")
	if len(got) != 1 || got[0].Title != "a" {
		t.Fatalf("board = %+v", got)
	}
	// Set replaces (last wins).
	_ = s.Set(ctx, "s", []TaskItem{{ID: "1", Title: "a", Status: "done"}, {ID: "2", Title: "b", Status: "pending"}})
	got, _ = s.Get(ctx, "s")
	if len(got) != 2 || got[0].Status != "done" {
		t.Fatalf("board after replace = %+v", got)
	}
}
