package tools

import (
	"context"
	"testing"

	"agentic/internal/eventlog"

	"google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

// partialTextEvent builds a streaming partial text event.
func partialTextEvent(text string) *session.Event {
	e := &session.Event{Author: "child"}
	e.Content = &genai.Content{Parts: []*genai.Part{{Text: text}}}
	e.Partial = true
	return e
}

func finalTextEvent(parts ...string) *session.Event {
	e := &session.Event{Author: "child"}
	ps := make([]*genai.Part, 0, len(parts))
	for _, p := range parts {
		ps = append(ps, &genai.Part{Text: p})
	}
	e.Content = &genai.Content{Parts: ps}
	return e
}

func TestTranslateChildEvent_PartialTextToAgentDelta(t *testing.T) {
	st := &childStreamState{}
	out := translateChildEvent(partialTextEvent("hel"), "researcher#ab12", "researcher", "sess:child", 3, st)
	if len(out) != 1 {
		t.Fatalf("want 1 event, got %d", len(out))
	}
	ev := out[0]
	if ev.Type != eventlog.EvAgentDelta || ev.Kind != eventlog.KindText {
		t.Fatalf("want agent-delta/text, got %s/%s", ev.Type, ev.Kind)
	}
	if ev.Author != "researcher#ab12" || ev.SubagentType != "researcher" || ev.SessionID != "sess:child" || ev.Step != 3 {
		t.Fatalf("bad attribution: %+v", ev)
	}
	if ev.Text != "hel" {
		t.Fatalf("want text 'hel', got %q", ev.Text)
	}
}

func TestTranslateChildEvent_ReasoningKind(t *testing.T) {
	st := &childStreamState{}
	e := &session.Event{Author: "child"}
	e.Content = &genai.Content{Parts: []*genai.Part{{Text: "thinking", Thought: true}}}
	e.Partial = true
	out := translateChildEvent(e, "l", "researcher", "cid", 1, st)
	if len(out) != 1 || out[0].Kind != eventlog.KindReasoning {
		t.Fatalf("want reasoning delta, got %+v", out)
	}
}

func TestTranslateChildEvent_MultiPartFinalTextJoined(t *testing.T) {
	st := &childStreamState{}
	// No prior partials: a final message with two text parts must be joined, not
	// truncated to the last part.
	translateChildEvent(finalTextEvent("Part A. ", "Part B."), "l", "researcher", "cid", 1, st)
	if st.finalText != "Part A. Part B." {
		t.Fatalf("multi-part final text not joined: %q", st.finalText)
	}
}

func TestTranslateChildEvent_FinalTextAfterPartialsNotDuplicated(t *testing.T) {
	st := &childStreamState{}
	// Stream two partials, then the non-partial full message.
	translateChildEvent(partialTextEvent("Hello "), "l", "researcher", "cid", 1, st)
	translateChildEvent(partialTextEvent("world"), "l", "researcher", "cid", 1, st)
	out := translateChildEvent(finalTextEvent("Hello world"), "l", "researcher", "cid", 1, st)
	// The final text already streamed as deltas; no new text delta should emit.
	for _, ev := range out {
		if ev.Type == eventlog.EvAgentDelta && ev.Kind == eventlog.KindText {
			t.Fatalf("final text duplicated as delta after partials: %+v", ev)
		}
	}
	if st.finalText != "Hello world" {
		t.Fatalf("finalText = %q", st.finalText)
	}
}

func TestTranslateChildEvent_ToolCallAndResult(t *testing.T) {
	st := &childStreamState{}
	e := &session.Event{Author: "child"}
	e.Content = &genai.Content{Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "web_search", Args: map[string]any{"q": "x"}}},
	}}
	out := translateChildEvent(e, "l", "researcher", "cid", 1, st)
	if len(out) != 1 || out[0].Type != eventlog.EvToolCall {
		t.Fatalf("want tool-call, got %+v", out)
	}
	if out[0].IsOutput {
		t.Fatalf("child tool call must NOT be IsOutput (renders as AgentToolCall)")
	}
	if out[0].Tool == nil || out[0].Tool.Name != "web_search" {
		t.Fatalf("bad tool payload: %+v", out[0].Tool)
	}

	r := &session.Event{Author: "child"}
	r.Content = &genai.Content{Parts: []*genai.Part{
		{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "web_search", Response: map[string]any{"ok": true}}},
	}}
	out2 := translateChildEvent(r, "l", "researcher", "cid", 1, st)
	if len(out2) != 1 || out2[0].Type != eventlog.EvToolResult || out2[0].IsOutput {
		t.Fatalf("want non-output tool-result, got %+v", out2)
	}
}

func TestTranslateChildEvent_ConfirmationSetsBlocked(t *testing.T) {
	st := &childStreamState{}
	e := &session.Event{Author: "child"}
	e.Content = &genai.Content{Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: "c1", Name: toolconfirmation.FunctionCallName}},
	}}
	out := translateChildEvent(e, "l", "researcher", "cid", 1, st)
	if !st.blocked {
		t.Fatalf("confirmation should set blocked")
	}
	if len(out) != 0 {
		t.Fatalf("confirmation should not emit a tool-call event, got %+v", out)
	}
}

func TestTaskHub_JoinFanIn(t *testing.T) {
	hub := NewTaskHub(2)
	hub.register("a")
	hub.register("b")

	go hub.finish("a", TaskResult{SessionID: "a", Status: "completed", Result: "RA"})
	go hub.finish("b", TaskResult{SessionID: "b", Status: "completed", Result: "RB"})

	ctx := context.Background()
	ra, ok := hub.wait(ctx, "a")
	if !ok || ra.Result != "RA" {
		t.Fatalf("join a: ok=%v res=%+v", ok, ra)
	}
	rb, ok := hub.wait(ctx, "b")
	if !ok || rb.Result != "RB" {
		t.Fatalf("join b: ok=%v res=%+v", ok, rb)
	}
	if _, ok := hub.wait(ctx, "unknown"); ok {
		t.Fatalf("wait on unknown id should return ok=false")
	}
}

func TestTaskHub_TaskListMonotonic(t *testing.T) {
	hub := NewTaskHub(0)
	hub.upsertTask("p", eventlog.TaskItem{ID: "c1", Title: "search", Status: "running", Agent: "researcher"})
	snap := hub.upsertTask("p", eventlog.TaskItem{ID: "c1", Status: "done"})
	if len(snap) != 1 || snap[0].Status != "done" || snap[0].Title != "search" {
		t.Fatalf("expected c1 done keeping title, got %+v", snap)
	}
	// A settled status must not regress.
	snap = hub.upsertTask("p", eventlog.TaskItem{ID: "c1", Status: "running"})
	if snap[0].Status != "done" {
		t.Fatalf("settled status regressed to %q", snap[0].Status)
	}
	// A second task appends.
	snap = hub.upsertTask("p", eventlog.TaskItem{ID: "c2", Title: "analyse", Status: "running"})
	if len(snap) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(snap))
	}
}
