// Package openai implements the stream.Encoder for the hybrid OpenAI
// chat.completion + ag_ui SSE wire format. It reproduces, byte-for-byte, the
// emit logic that previously lived inline in internal/agent.streamEvents and its
// write* helpers. Event-struct construction is identical to the originals; only
// the transport call is swapped (sse.WriteSSE → sink.Send, sse.WriteDone →
// sink.SendRaw("[DONE]")).
package openai

import (
	"encoding/json"
	"fmt"
	"time"

	"agentic/internal/sse"
	"agentic/internal/stream"
	"agentic/internal/types"

	"github.com/google/uuid"
)

// Encoder turns semantic stream events into the OpenAI + ag_ui SSE wire format.
type Encoder struct {
	sink     stream.Sink
	cb       *sse.ChunkBuilder
	threadID string
	runID    string
}

var _ stream.Encoder = (*Encoder)(nil)

// New builds an Encoder over sink. requestID becomes both the ChunkBuilder
// request id and the ag_ui RunID; model is the OpenAI model field.
func New(sink stream.Sink, requestID, model, threadID string) *Encoder {
	return &Encoder{
		sink:     sink,
		cb:       sse.NewChunkBuilder(requestID, model, threadID),
		threadID: threadID,
		runID:    requestID,
	}
}

// writeAGUI mirrors agent.writeAGUI.
func (e *Encoder) writeAGUI(event *types.AGUIEvent) {
	e.sink.Send(map[string]any{"ag_ui": event})
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

func (e *Encoder) RunStarted() {
	e.writeAGUI(&types.AGUIEvent{
		Type:      "RUN_STARTED",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
	})
}

func (e *Encoder) RunFinished(u stream.Usage) {
	e.writeAGUI(&types.AGUIEvent{
		Type:      "RUN_FINISHED",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
	})
	if u.Total > 0 {
		e.sink.Send(e.cb.FinishWithUsage("stop", u.Prompt, u.Completion, u.Total))
	} else {
		e.sink.Send(e.cb.Finish("stop"))
	}
	e.sink.SendRaw("[DONE]")
}

// ── Run-level progress ───────────────────────────────────────────────────────

// Progress ports agent.writeProgress.
func (e *Encoder) Progress(phase, message string) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
	evt.AGUI = &types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		Name:      "agent_progress",
		Value: map[string]any{
			"phase":   phase,
			"message": message,
		},
	}
	e.sink.Send(evt)
}

// ── Output (main-thread) agent ───────────────────────────────────────────────

func (e *Encoder) Text(delta string) {
	e.sink.Send(e.cb.TextDelta(delta))
}

func (e *Encoder) Reasoning(delta string) {
	e.sink.Send(e.cb.ReasoningDelta(delta))
}

func (e *Encoder) ToolCall(index int64, id, name, argsJSON string) {
	e.sink.Send(e.cb.ToolCallDelta(index, id, name, argsJSON))
	e.sink.Send(e.cb.Finish("tool_calls"))
}

// ToolResult mirrors the output-agent (isOutputAgent) tool-result branch in
// agent.streamEvents.
func (e *Encoder) ToolResult(id, name string, response any) {
	content, _ := json.Marshal(response)
	evt := types.ToolResultEvent{}
	evt.ToolResult.ToolCallID = id
	evt.ToolResult.ToolName = name
	evt.ToolResult.Result = response
	evt.AGUI = &types.AGUIEvent{
		Type:         "TOOL_CALL_RESULT",
		Timestamp:    time.Now().UnixMilli(),
		ThreadID:     e.threadID,
		RunID:        e.runID,
		MessageID:    fmt.Sprintf("tool-%s", id),
		ToolCallID:   id,
		ToolCallName: name,
		Content:      string(content),
	}
	e.sink.Send(evt)
}

// ── Sub-agents ───────────────────────────────────────────────────────────────

// writeAgentProgress ports agent.writeAgentProgress.
func (e *Encoder) writeAgentProgress(phase, message, agentName string, step int) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
	evt.AgentProgress.Agent = agentName
	evt.AgentProgress.Step = step
	aguiType := "STEP_STARTED"
	if phase == "agent_done" {
		aguiType = "STEP_FINISHED"
	}
	evt.AGUI = &types.AGUIEvent{
		Type:      aguiType,
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		StepName:  agentName,
		RawEvent: map[string]any{
			"phase":   phase,
			"message": message,
			"step":    step,
		},
	}
	e.sink.Send(evt)
}

func (e *Encoder) AgentStart(agent string, step int) {
	e.writeAgentProgress("agent_start", fmt.Sprintf("Running %s...", agent), agent, step)
}

// AgentDone ports agent.writeAgentDone.
func (e *Encoder) AgentDone(agent string, step int, durationMs int64) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = "agent_done"
	evt.AgentProgress.Message = fmt.Sprintf("%s completed", agent)
	evt.AgentProgress.Agent = agent
	evt.AgentProgress.Step = step
	evt.AgentProgress.DurationMs = durationMs
	evt.AGUI = &types.AGUIEvent{
		Type:      "STEP_FINISHED",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		StepName:  agent,
		RawEvent: map[string]any{
			"phase":       "agent_done",
			"step":        step,
			"duration_ms": durationMs,
		},
	}
	e.sink.Send(evt)
}

func (e *Encoder) AgentProgress(phase, message, agent string, step int) {
	e.writeAgentProgress(phase, message, agent, step)
}

// writeAgentEvent ports agent.writeAgentEvent.
func (e *Encoder) writeAgentEvent(agentName, eventType, content string, step int) {
	evt := types.AgentEventEvent{}
	evt.AgentEvent.Agent = agentName
	evt.AgentEvent.Type = eventType
	evt.AgentEvent.Content = content
	evt.AgentEvent.Step = step
	aguiType := "CUSTOM"
	if eventType == "text_delta" {
		aguiType = "TEXT_MESSAGE_CONTENT"
	} else if eventType == "text_done" {
		aguiType = "TEXT_MESSAGE_END"
	}
	evt.AGUI = &types.AGUIEvent{
		Type:      aguiType,
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		MessageID: fmt.Sprintf("%s-%d", agentName, step),
		Delta:     content,
		RawEvent: map[string]any{
			"agent": agentName,
			"type":  eventType,
			"step":  step,
		},
	}
	e.sink.Send(evt)
}

func (e *Encoder) AgentText(agent string, step int, delta string) {
	e.writeAgentEvent(agent, "text_delta", delta, step)
}

func (e *Encoder) AgentTextDone(agent string, step int) {
	e.writeAgentEvent(agent, "text_done", "", step)
}

// AgentReasoning ports agent.writeReasoning.
func (e *Encoder) AgentReasoning(agent string, step int, delta string) {
	evt := types.AgentEventEvent{}
	evt.AgentEvent.Agent = agent
	evt.AgentEvent.Type = "reasoning_delta"
	evt.AgentEvent.ReasoningContent = delta
	evt.AgentEvent.Step = step
	evt.AGUI = &types.AGUIEvent{
		Type:      "THINKING_TEXT_MESSAGE_CONTENT",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		MessageID: fmt.Sprintf("%s-%d", agent, step),
		Delta:     delta,
		RawEvent: map[string]any{
			"agent": agent,
			"type":  "reasoning_delta",
			"step":  step,
		},
	}
	e.sink.Send(evt)
}

// AgentToolCall ports agent.writeAgentToolCall.
func (e *Encoder) AgentToolCall(agent string, step int, name, id, argsJSON string) {
	evt := types.AgentEventEvent{}
	evt.AgentEvent.Agent = agent
	evt.AgentEvent.Type = "tool_call"
	evt.AgentEvent.ToolName = name
	evt.AgentEvent.ToolCallID = id
	evt.AgentEvent.Content = argsJSON
	evt.AgentEvent.Step = step
	evt.AGUI = &types.AGUIEvent{
		Type:         "TOOL_CALL_START",
		Timestamp:    time.Now().UnixMilli(),
		ThreadID:     e.threadID,
		RunID:        e.runID,
		MessageID:    fmt.Sprintf("%s-%d", agent, step),
		ToolCallID:   id,
		ToolCallName: name,
	}
	e.sink.Send(evt)
}

// AgentToolResult ports agent.writeAgentToolResult.
func (e *Encoder) AgentToolResult(agent string, step int, name, id, content string) {
	evt := types.AgentEventEvent{}
	evt.AgentEvent.Agent = agent
	evt.AgentEvent.Type = "tool_result"
	evt.AgentEvent.ToolName = name
	evt.AgentEvent.ToolCallID = id
	evt.AgentEvent.Content = content
	evt.AgentEvent.Step = step
	evt.AGUI = &types.AGUIEvent{
		Type:         "TOOL_CALL_RESULT",
		Timestamp:    time.Now().UnixMilli(),
		ThreadID:     e.threadID,
		RunID:        e.runID,
		MessageID:    fmt.Sprintf("%s-%d", agent, step),
		ToolCallID:   id,
		ToolCallName: name,
		Content:      content,
	}
	e.sink.Send(evt)
}

// ── Data parts ───────────────────────────────────────────────────────────────

// Artifact ports agent.writeArtifact. value is the raw emit_artifact tool
// response map.
func (e *Encoder) Artifact(value map[string]any) {
	str := func(key string) string {
		if v, ok := value[key].(string); ok {
			return v
		}
		return ""
	}
	id := str("id")
	if id == "" {
		id = uuid.New().String()
	}
	v := map[string]any{
		"id":      id,
		"title":   str("title"),
		"kind":    str("kind"),
		"content": str("content"),
	}
	if lang := str("language"); lang != "" {
		v["language"] = lang
	}
	e.writeAGUI(&types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		Name:      "artifact",
		Value:     v,
	})
}

// TaskList ports agent.writeTaskList. stream.Task carries omitempty json tags
// for priority/agent, so marshalling the slice directly reproduces the original
// per-task maps.
func (e *Encoder) TaskList(tasks []stream.Task) {
	e.writeAGUI(&types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		Name:      "task_list",
		Value:     map[string]any{"tasks": tasks},
	})
}

// Usage ports agent.writeContextUsage. The breakdown is built from the passed
// buckets.
func (e *Encoder) Usage(u stream.Usage, breakdown []stream.Bucket) {
	rows := make([]map[string]any, 0, len(breakdown))
	for _, b := range breakdown {
		rows = append(rows, map[string]any{"label": b.Label, "tokens": b.Tokens})
	}
	e.writeAGUI(&types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  e.threadID,
		RunID:     e.runID,
		Name:      "context_usage",
		Value: map[string]any{
			"prompt_tokens":     u.Prompt,
			"completion_tokens": u.Completion,
			"total_tokens":      u.Total,
			"context_used":      u.Prompt,
			"context_window":    u.ContextWindow,
			"breakdown":         rows,
		},
	})
}

// ── HITL ─────────────────────────────────────────────────────────────────────

// ToolInterrupt ports the emit side of the HITL block in agent.streamEvents.
func (e *Encoder) ToolInterrupt(i stream.Interrupt) {
	argsJSON, _ := json.Marshal(i.Details)
	e.sink.Send(e.cb.ToolCallDelta(0, i.ToolCallID, i.ToolName, string(argsJSON)))
	e.sink.Send(e.cb.Finish("tool_calls"))

	evt := types.ToolInterruptEvent{}
	evt.ToolInterrupt.ToolCallID = i.ToolCallID
	evt.ToolInterrupt.ToolName = i.ToolName
	evt.ToolInterrupt.Prompt = i.Prompt
	evt.ToolInterrupt.Details = i.Details
	evt.ToolInterrupt.ThreadID = i.ThreadID
	evt.AGUI = &types.AGUIEvent{
		Type:         "CUSTOM",
		Timestamp:    time.Now().UnixMilli(),
		ThreadID:     e.threadID,
		RunID:        e.runID,
		Name:         "tool_interrupt",
		ToolCallID:   i.ToolCallID,
		ToolCallName: i.ToolName,
		Value: map[string]any{
			"prompt":  i.Prompt,
			"details": i.Details,
		},
	}
	e.sink.Send(evt)

	e.sink.Send(e.cb.Finish("stop"))
	e.sink.SendRaw("[DONE]")
}

// ── Metadata ─────────────────────────────────────────────────────────────────

// Metadata is a no-op: the OpenAI wire format has no metadata channel.
func (e *Encoder) Metadata(model, agentID string, durationMs int64) {}
