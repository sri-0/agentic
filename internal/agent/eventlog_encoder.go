package agent

import (
	"context"

	"agentic/internal/eventlog"
	"agentic/internal/stream"
)

// eventLogEncoder implements stream.Encoder by serialising each encoder call as
// a typed AgentEvent appended to an EventLog for one session. This lets the
// existing streamEvents translation pipeline populate the durable log unchanged:
// the background run drives streamEvents into this encoder, and PumpEventLog
// (the read side) replays the AgentEvents back through a real SSE encoder.
//
// It is driven by a single goroutine (the run loop), so appends are serialised.
type eventLogEncoder struct {
	ctx         context.Context
	log         eventlog.EventLog
	sessionID   string
	interrupted bool // set when the run paused on a HITL/question interrupt
}

// Interrupted reports whether the run suspended on a tool interrupt (HITL /
// question), so the coordinator can mark the run awaiting-input rather than done.
func (e *eventLogEncoder) Interrupted() bool { return e.interrupted }

func newEventLogEncoder(ctx context.Context, log eventlog.EventLog, sessionID string) *eventLogEncoder {
	return &eventLogEncoder{ctx: ctx, log: log, sessionID: sessionID}
}

func (e *eventLogEncoder) put(ev eventlog.AgentEvent) {
	ev.V = 1
	_, _ = e.log.Append(e.ctx, e.sessionID, ev)
}

// Lifecycle. RunStarted framing is owned by the pump; RunFinished's usage is
// carried as a final usage event, and the coordinator appends the terminal
// run-status separately.
func (e *eventLogEncoder) RunStarted() {}

func (e *eventLogEncoder) RunFinished(u stream.Usage) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvUsage, Usage: usagePayload(u, nil), IsOutput: true})
}

func (e *eventLogEncoder) Progress(phase, message string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvProgress, Phase: phase, Message: message})
}

func (e *eventLogEncoder) Text(delta string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvTextDelta, Text: delta, IsOutput: true})
}

func (e *eventLogEncoder) Reasoning(delta string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvReasoningDelta, Text: delta, IsOutput: true})
}

func (e *eventLogEncoder) ToolCall(index int64, id, name, argsJSON string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvToolCall, IsOutput: true, Step: int(index),
		Tool: &eventlog.ToolPayload{ID: id, Name: name, Args: argsJSON}})
}

func (e *eventLogEncoder) ToolResult(id, name string, response any) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvToolResult, IsOutput: true,
		Tool: &eventlog.ToolPayload{ID: id, Name: name, Result: response}})
}

func (e *eventLogEncoder) AgentStart(agent string, step int) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvAgentStep, Kind: eventlog.KindStarted, Author: agent, Step: step})
}

func (e *eventLogEncoder) AgentDone(agent string, step int, durationMs int64) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvAgentStep, Kind: eventlog.KindDone, Author: agent, Step: step, Duration: durationMs})
}

func (e *eventLogEncoder) AgentProgress(phase, message, agent string, step int) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvProgress, Phase: phase, Message: message, Author: agent, Step: step})
}

func (e *eventLogEncoder) AgentText(agent string, step int, delta string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvAgentDelta, Kind: eventlog.KindText, Author: agent, Step: step, Text: delta})
}

func (e *eventLogEncoder) AgentTextDone(agent string, step int) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvAgentDelta, Kind: eventlog.KindTextDone, Author: agent, Step: step})
}

func (e *eventLogEncoder) AgentReasoning(agent string, step int, delta string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvAgentDelta, Kind: eventlog.KindReasoning, Author: agent, Step: step, Text: delta})
}

func (e *eventLogEncoder) AgentToolCall(agent string, step int, name, id, argsJSON string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvToolCall, Author: agent, Step: step,
		Tool: &eventlog.ToolPayload{ID: id, Name: name, Args: argsJSON}})
}

func (e *eventLogEncoder) AgentToolResult(agent string, step int, name, id, content string) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvToolResult, Author: agent, Step: step,
		Tool: &eventlog.ToolPayload{ID: id, Name: name, Result: content}})
}

func (e *eventLogEncoder) Artifact(value map[string]any) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvArtifact, Artifact: value})
}

func (e *eventLogEncoder) TaskList(tasks []stream.Task) {
	items := make([]eventlog.TaskItem, len(tasks))
	for i, t := range tasks {
		items[i] = eventlog.TaskItem{ID: t.ID, Title: t.Title, Status: t.Status, Priority: t.Priority, Agent: t.Agent}
	}
	e.put(eventlog.AgentEvent{Type: eventlog.EvTaskList, Tasks: items})
}

func (e *eventLogEncoder) Usage(u stream.Usage, breakdown []stream.Bucket) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvUsage, Usage: usagePayload(u, breakdown)})
}

func (e *eventLogEncoder) ToolInterrupt(i stream.Interrupt) {
	e.interrupted = true
	e.put(eventlog.AgentEvent{Type: eventlog.EvQuestion, Question: &eventlog.QuestionPayload{
		ToolCallID: i.ToolCallID, ToolName: i.ToolName, Prompt: i.Prompt,
		Details: map[string]any{"details": i.Details, "thread_id": i.ThreadID},
	}})
}

func (e *eventLogEncoder) Metadata(model, agentID string, durationMs int64) {
	e.put(eventlog.AgentEvent{Type: eventlog.EvMetadata, Model: model, Author: agentID, Duration: durationMs})
}

func usagePayload(u stream.Usage, breakdown []stream.Bucket) *eventlog.UsagePayload {
	p := &eventlog.UsagePayload{
		PromptTokens:     u.Prompt,
		CompletionTokens: u.Completion,
		TotalTokens:      u.Total,
		ContextWindow:    u.ContextWindow,
	}
	for _, b := range breakdown {
		p.Breakdown = append(p.Breakdown, eventlog.UsageBucket{Label: b.Label, Tokens: b.Tokens})
	}
	return p
}

// compile-time check
var _ stream.Encoder = (*eventLogEncoder)(nil)
