package eventlog

import (
	"encoding/json"
	"testing"
)

// findPart returns the first part of the given type, or (Part{}, false).
func findPart(parts []Part, typ string) (Part, bool) {
	for _, p := range parts {
		if p.Type == typ {
			return p, true
		}
	}
	return Part{}, false
}

func countParts(parts []Part, typ string) int {
	n := 0
	for _, p := range parts {
		if p.Type == typ {
			n++
		}
	}
	return n
}

func TestProjectMessages_CoalescesTextDeltas(t *testing.T) {
	events := []AgentEvent{
		{Type: EvRunStatus, Status: StatusRunning},
		{Type: EvTextDelta, Text: "Hello ", IsOutput: true},
		{Type: EvTextDelta, Text: "world", IsOutput: true},
		{Type: EvTextDelta, Text: "!", IsOutput: true},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Role != "assistant" {
		t.Errorf("role = %q, want assistant", m.Role)
	}
	if m.Content != "Hello world!" {
		t.Errorf("content = %q", m.Content)
	}
	if n := countParts(m.Parts, PartText); n != 1 {
		t.Fatalf("want exactly 1 coalesced text part, got %d", n)
	}
	tp, _ := findPart(m.Parts, PartText)
	if tp.Text != "Hello world!" {
		t.Errorf("text part = %q", tp.Text)
	}
}

func TestProjectMessages_ReasoningPart(t *testing.T) {
	events := []AgentEvent{
		{Type: EvReasoningDelta, Text: "think", IsOutput: true},
		{Type: EvReasoningDelta, Text: "ing", IsOutput: true},
		{Type: EvTextDelta, Text: "answer", IsOutput: true},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	rp, ok := findPart(msgs[0].Parts, PartReasoning)
	if !ok {
		t.Fatal("no reasoning part")
	}
	if rp.Text != "thinking" {
		t.Errorf("reasoning = %q", rp.Text)
	}
	// reasoning must precede text (order preserved).
	if msgs[0].Parts[0].Type != PartReasoning || msgs[0].Parts[1].Type != PartText {
		t.Errorf("part order = %v", partTypes(msgs[0].Parts))
	}
	// reasoning does not leak into flattened content.
	if msgs[0].Content != "answer" {
		t.Errorf("content = %q, reasoning should not be flattened", msgs[0].Content)
	}
}

func TestProjectMessages_ToolCallResultPaired(t *testing.T) {
	events := []AgentEvent{
		{Type: EvTextDelta, Text: "let me search", IsOutput: true},
		{Type: EvToolCall, IsOutput: true, Tool: &ToolPayload{ID: "call_1", Name: "search", Args: `{"q":"cats"}`}},
		{Type: EvToolResult, IsOutput: true, Tool: &ToolPayload{ID: "call_1", Name: "search", Result: map[string]any{"hits": 3}}},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	tp, ok := findPart(msgs[0].Parts, PartDynamicTool)
	if !ok {
		t.Fatal("no dynamic-tool part")
	}
	if tp.ToolName != "search" || tp.ToolCallID != "call_1" {
		t.Errorf("tool part meta = %+v", tp)
	}
	if tp.State != "output-available" {
		t.Errorf("state = %q, want output-available", tp.State)
	}
	// input parsed from the JSON string into an object.
	in, ok := tp.Input.(map[string]any)
	if !ok || in["q"] != "cats" {
		t.Errorf("input = %#v, want parsed object", tp.Input)
	}
	out, ok := tp.Output.(map[string]any)
	if !ok || out["hits"].(int) != 3 {
		t.Errorf("output = %#v", tp.Output)
	}
	if n := countParts(msgs[0].Parts, PartDynamicTool); n != 1 {
		t.Errorf("want 1 tool part (paired), got %d", n)
	}
}

func TestProjectMessages_UnresolvedToolCall(t *testing.T) {
	events := []AgentEvent{
		{Type: EvToolCall, IsOutput: true, Tool: &ToolPayload{ID: "c1", Name: "run", Args: `{}`}},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	tp, _ := findPart(msgs[0].Parts, PartDynamicTool)
	if tp.State != "input-available" {
		t.Errorf("state = %q, want input-available for unresolved call", tp.State)
	}
}

func TestProjectMessages_ArtifactPart(t *testing.T) {
	events := []AgentEvent{
		{Type: EvArtifact, Artifact: map[string]any{"id": "art1", "title": "Report", "kind": "markdown", "content": "# Hi"}},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	ap, ok := findPart(msgs[0].Parts, PartArtifact)
	if !ok {
		t.Fatal("no artifact part")
	}
	if ap.ID != "art1" {
		t.Errorf("artifact id = %q", ap.ID)
	}
	d := ap.Data.(map[string]any)
	if d["title"] != "Report" || d["content"] != "# Hi" || d["kind"] != "markdown" {
		t.Errorf("artifact data = %#v", d)
	}
}

func TestProjectMessages_TaskListLastWins(t *testing.T) {
	events := []AgentEvent{
		{Type: EvTaskList, Tasks: []TaskItem{{ID: "t1", Title: "a", Status: "pending"}}},
		{Type: EvTaskList, Tasks: []TaskItem{
			{ID: "t1", Title: "a", Status: "done"},
			{ID: "t2", Title: "b", Status: "in_progress"},
		}},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	if n := countParts(msgs[0].Parts, PartTaskList); n != 1 {
		t.Fatalf("want 1 task-list part (last-wins), got %d", n)
	}
	tp, _ := findPart(msgs[0].Parts, PartTaskList)
	if tp.ID != "tasks" {
		t.Errorf("task-list id = %q, want tasks", tp.ID)
	}
	d := tp.Data.(map[string]any)
	rows := d["tasks"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("want last snapshot with 2 tasks, got %d", len(rows))
	}
	if rows[0]["status"] != "done" {
		t.Errorf("t1 status = %v, want done (last snapshot)", rows[0]["status"])
	}
}

func TestProjectMessages_SubAgentParts(t *testing.T) {
	events := []AgentEvent{
		{Type: EvAgentStep, Kind: KindStarted, Author: "researcher#ab12", Step: 1},
		{Type: EvAgentDelta, Kind: KindText, Author: "researcher#ab12", Step: 1, Text: "found it"},
		{Type: EvAgentDelta, Kind: KindReasoning, Author: "researcher#ab12", Step: 1, Text: "hmm"},
		{Type: EvAgentStep, Kind: KindDone, Author: "researcher#ab12", Step: 1, Duration: 1200},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	if countParts(msgs[0].Parts, PartAgentStep) != 2 {
		t.Errorf("want 2 agent-step parts, got %d", countParts(msgs[0].Parts, PartAgentStep))
	}
	if countParts(msgs[0].Parts, PartAgentDelta) != 2 {
		t.Errorf("want 2 agent-delta parts, got %d", countParts(msgs[0].Parts, PartAgentDelta))
	}
	step, _ := findPart(msgs[0].Parts, PartAgentStep)
	d := step.Data.(map[string]any)
	if d["agent"] != "researcher#ab12" || d["status"] != "started" {
		t.Errorf("agent-step data = %#v", d)
	}
	if step.ID != "researcher#ab12-1" {
		t.Errorf("agent-step id = %q", step.ID)
	}
}

func TestProjectMessages_MultiTurn(t *testing.T) {
	events := []AgentEvent{
		// turn 1
		{Type: EvRunStatus, Status: StatusRunning},
		{Type: EvTextDelta, Text: "one", IsOutput: true},
		{Type: EvRunStatus, Status: StatusDone},
		// turn 2
		{Type: EvRunStatus, Status: StatusRunning},
		{Type: EvTextDelta, Text: "two", IsOutput: true},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	if len(msgs) != 2 {
		t.Fatalf("want 2 assistant messages, got %d", len(msgs))
	}
	if msgs[0].Content != "one" || msgs[1].Content != "two" {
		t.Errorf("contents = %q, %q", msgs[0].Content, msgs[1].Content)
	}
}

func TestProjectMessages_AwaitingInputKeepsMessageOpen(t *testing.T) {
	events := []AgentEvent{
		{Type: EvRunStatus, Status: StatusRunning},
		{Type: EvTextDelta, Text: "before ", IsOutput: true},
		{Type: EvRunStatus, Status: StatusAwaitingInput}, // suspend, do NOT flush
		{Type: EvRunStatus, Status: StatusRunning},        // resume same session
		{Type: EvTextDelta, Text: "after", IsOutput: true},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	if len(msgs) != 1 {
		t.Fatalf("awaiting-input should not split the message; got %d messages", len(msgs))
	}
	if msgs[0].Content != "before after" {
		t.Errorf("content = %q, want continuous", msgs[0].Content)
	}
}

func TestProjectMessages_SubAgentTextNotFlattened(t *testing.T) {
	// sub-agent deltas (IsOutput=false) must not pollute the assistant text.
	events := []AgentEvent{
		{Type: EvTextDelta, Text: "main", IsOutput: true},
		{Type: EvAgentDelta, Kind: KindText, Author: "worker", Step: 1, Text: "child text"},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	if msgs[0].Content != "main" {
		t.Errorf("content = %q, sub-agent text must not flatten", msgs[0].Content)
	}
}

func TestProjectMessages_Empty(t *testing.T) {
	if got := ProjectMessages(nil); len(got) != 0 {
		t.Errorf("nil events -> %d messages, want 0", len(got))
	}
	// a run with no output produces no message.
	events := []AgentEvent{
		{Type: EvRunStatus, Status: StatusRunning},
		{Type: EvRunStatus, Status: StatusError, Err: "boom"},
	}
	if got := ProjectMessages(events); len(got) != 0 {
		t.Errorf("empty run -> %d messages, want 0", len(got))
	}
}

// TestProjectMessages_JSONShape asserts the marshalled part JSON matches the
// AI-SDK shapes the live aisdk encoder emits (so a reload renders identically).
func TestProjectMessages_JSONShape(t *testing.T) {
	events := []AgentEvent{
		{Type: EvTextDelta, Text: "hi", IsOutput: true},
		{Type: EvToolCall, IsOutput: true, Tool: &ToolPayload{ID: "c1", Name: "search", Args: `{"q":"x"}`}},
		{Type: EvToolResult, IsOutput: true, Tool: &ToolPayload{ID: "c1", Name: "search", Result: "done"}},
		{Type: EvRunStatus, Status: StatusDone},
	}
	msgs := ProjectMessages(events)
	b, err := json.Marshal(msgs[0].Parts)
	if err != nil {
		t.Fatal(err)
	}
	var parts []map[string]any
	if err := json.Unmarshal(b, &parts); err != nil {
		t.Fatal(err)
	}
	// text part
	if parts[0]["type"] != "text" || parts[0]["text"] != "hi" {
		t.Errorf("text part json = %#v", parts[0])
	}
	// dynamic-tool part
	tp := parts[1]
	if tp["type"] != "dynamic-tool" || tp["toolName"] != "search" || tp["toolCallId"] != "c1" || tp["state"] != "output-available" {
		t.Errorf("tool part json = %#v", tp)
	}
	if in, ok := tp["input"].(map[string]any); !ok || in["q"] != "x" {
		t.Errorf("tool input json = %#v", tp["input"])
	}
	if tp["output"] != "done" {
		t.Errorf("tool output json = %#v", tp["output"])
	}
}

func partTypes(parts []Part) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.Type
	}
	return out
}
