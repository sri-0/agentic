// Package aisdk implements the Vercel AI SDK v6 "UI Message Stream" wire format.
//
// It mirrors the frontend translation in
// agentui/lib/chat/pump-backend-sse.ts: each Encoder method emits one or more
// SSE chunks (`data: {json}\n\n`) whose shapes match exactly what the UI message
// writer produces. The run is framed by a leading `{"type":"start"}` and a
// trailing `{"type":"finish"}` followed by the `[DONE]` sentinel.
package aisdk

import (
	"encoding/json"
	"fmt"

	"agentic/internal/stream"
)

var _ stream.Encoder = (*Encoder)(nil)

// Encoder translates semantic stream events into AI-SDK UI-message chunks.
// Not safe for concurrent use — methods are called in order from one event loop.
type Encoder struct {
	sink    stream.Sink
	model   string
	agentID string

	seq         int
	textID      string
	reasoningID string
}

// New builds an Encoder writing to sink. model and agentID are stamped onto the
// message-metadata chunks.
func New(sink stream.Sink, model, agentID string) *Encoder {
	return &Encoder{sink: sink, model: model, agentID: agentID}
}

func (e *Encoder) nextID(prefix string) string {
	id := fmt.Sprintf("%s-%d", prefix, e.seq)
	e.seq++
	return id
}

func (e *Encoder) send(v any) {
	_ = e.sink.Send(v)
}

// ── text / reasoning open-part management ────────────────────────────────────

func (e *Encoder) ensureText() {
	if e.textID == "" {
		e.textID = e.nextID("txt")
		e.send(map[string]any{"type": "text-start", "id": e.textID})
	}
}

func (e *Encoder) closeText() {
	if e.textID != "" {
		e.send(map[string]any{"type": "text-end", "id": e.textID})
		e.textID = ""
	}
}

func (e *Encoder) ensureReasoning() {
	if e.reasoningID == "" {
		e.reasoningID = e.nextID("rsn")
		e.send(map[string]any{"type": "reasoning-start", "id": e.reasoningID})
	}
}

func (e *Encoder) closeReasoning() {
	if e.reasoningID != "" {
		e.send(map[string]any{"type": "reasoning-end", "id": e.reasoningID})
		e.reasoningID = ""
	}
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// RunStarted emits the leading start frame.
func (e *Encoder) RunStarted() {
	e.send(map[string]any{"type": "start"})
}

// RunFinished closes any open part, emits the terminal metadata + finish + [DONE].
func (e *Encoder) RunFinished(u stream.Usage) {
	e.closeReasoning()
	e.closeText()
	e.send(map[string]any{"type": "finish"})
	_ = e.sink.SendRaw("[DONE]")
}

// ── run-level progress ───────────────────────────────────────────────────────

// Progress emits an un-attributed (run-level) progress data part.
func (e *Encoder) Progress(phase, message string) {
	e.send(map[string]any{
		"type": "data-agent-progress",
		"id":   e.nextID("prog"),
		"data": map[string]any{"phase": phase, "message": message},
	})
}

// ── main-thread output ───────────────────────────────────────────────────────

// Text streams an assistant text delta, closing any open reasoning part first.
func (e *Encoder) Text(delta string) {
	e.closeReasoning()
	e.ensureText()
	e.send(map[string]any{"type": "text-delta", "id": e.textID, "delta": delta})
}

// Reasoning streams a reasoning delta, closing any open text part first.
func (e *Encoder) Reasoning(delta string) {
	e.closeText()
	e.ensureReasoning()
	e.send(map[string]any{"type": "reasoning-delta", "id": e.reasoningID, "delta": delta})
}

// ToolCall surfaces a completed main-thread tool call.
func (e *Encoder) ToolCall(index int64, id, name, argsJSON string) {
	e.closeText()
	e.send(map[string]any{
		"type":       "tool-input-start",
		"toolCallId": id,
		"toolName":   name,
		"dynamic":    true,
	})
	e.send(map[string]any{
		"type":           "tool-input-delta",
		"toolCallId":     id,
		"inputTextDelta": argsJSON,
	})
	e.send(map[string]any{
		"type":       "tool-input-available",
		"toolCallId": id,
		"toolName":   name,
		"input":      parseJSON(argsJSON),
		"dynamic":    true,
	})
}

// ToolResult surfaces a main-thread tool result.
func (e *Encoder) ToolResult(id, name string, response any) {
	e.send(map[string]any{
		"type":       "tool-output-available",
		"toolCallId": id,
		"output":     response,
		"dynamic":    true,
	})
}

// ── sub-agents ───────────────────────────────────────────────────────────────

// AgentStart marks a sub-agent step as started.
func (e *Encoder) AgentStart(agent string, step int) {
	e.send(map[string]any{
		"type": "data-agent-step",
		"id":   fmt.Sprintf("%s-%d", agent, step),
		"data": map[string]any{"agent": agent, "step": step, "status": "started"},
	})
}

// AgentDone marks a sub-agent step as done.
func (e *Encoder) AgentDone(agent string, step int, durationMs int64) {
	e.send(map[string]any{
		"type": "data-agent-step",
		"id":   fmt.Sprintf("%s-%d", agent, step),
		"data": map[string]any{
			"agent":      agent,
			"step":       step,
			"status":     "done",
			"durationMs": durationMs,
		},
	})
}

// AgentProgress emits an attributed progress data part.
func (e *Encoder) AgentProgress(phase, message, agent string, step int) {
	data := map[string]any{"phase": phase, "message": message}
	if agent != "" {
		data["agent"] = agent
	}
	e.send(map[string]any{
		"type": "data-agent-progress",
		"id":   e.nextID("prog"),
		"data": data,
	})
}

// AgentText streams a sub-agent text delta.
func (e *Encoder) AgentText(agent string, step int, delta string) {
	e.send(map[string]any{
		"type": "data-agent-delta",
		"id":   e.nextID("adelta"),
		"data": map[string]any{"agent": agent, "step": step, "kind": "text", "delta": delta},
	})
}

// AgentTextDone is a no-op — text completion is surfaced via progress only.
func (e *Encoder) AgentTextDone(agent string, step int) {}

// AgentReasoning streams a sub-agent reasoning delta.
func (e *Encoder) AgentReasoning(agent string, step int, delta string) {
	e.send(map[string]any{
		"type": "data-agent-delta",
		"id":   e.nextID("adelta"),
		"data": map[string]any{"agent": agent, "step": step, "kind": "reasoning", "delta": delta},
	})
}

// AgentToolCall is a no-op — sub-agent tool activity surfaces via AgentProgress.
func (e *Encoder) AgentToolCall(agent string, step int, name, id, argsJSON string) {}

// AgentToolResult is a no-op — sub-agent tool activity surfaces via AgentProgress.
func (e *Encoder) AgentToolResult(agent string, step int, name, id, content string) {}

// ── data parts ───────────────────────────────────────────────────────────────

// Artifact emits an artifact data part extracted from the raw response map.
func (e *Encoder) Artifact(value map[string]any) {
	id := str(value, "id")
	if id == "" {
		id = "artifact"
	}
	kind := str(value, "kind")
	if kind == "" {
		kind = "markdown"
	}
	data := map[string]any{
		"id":      id,
		"title":   str(value, "title"),
		"kind":    kind,
		"content": str(value, "content"),
	}
	if lang := str(value, "language"); lang != "" {
		data["language"] = lang
	}
	e.send(map[string]any{"type": "data-artifact", "id": id, "data": data})
}

// TaskList emits the live task-board snapshot.
func (e *Encoder) TaskList(tasks []stream.Task) {
	if tasks == nil {
		tasks = []stream.Task{} // never serialize null — the UI iterates this
	}
	e.send(map[string]any{
		"type": "data-task-list",
		"id":   "tasks",
		"data": map[string]any{"tasks": tasks},
	})
}

// Usage emits the context-usage data part.
func (e *Encoder) Usage(u stream.Usage, breakdown []stream.Bucket) {
	e.send(map[string]any{
		"type": "data-usage",
		"id":   "usage",
		"data": map[string]any{
			"promptTokens":     u.Prompt,
			"completionTokens": u.Completion,
			"totalTokens":      u.Total,
			"contextUsed":      u.Prompt,
			"contextWindow":    u.ContextWindow,
		},
	})
}

// ── HITL ─────────────────────────────────────────────────────────────────────

// ToolInterrupt surfaces the interrupted tool, advertises the approval request,
// then closes the stream — the run pauses here.
func (e *Encoder) ToolInterrupt(i stream.Interrupt) {
	e.closeText()

	detailsJSON, _ := json.Marshal(i.Details)
	e.send(map[string]any{
		"type":       "tool-input-start",
		"toolCallId": i.ToolCallID,
		"toolName":   i.ToolName,
		"dynamic":    true,
	})
	e.send(map[string]any{
		"type":           "tool-input-delta",
		"toolCallId":     i.ToolCallID,
		"inputTextDelta": string(detailsJSON),
	})
	e.send(map[string]any{
		"type":       "tool-input-available",
		"toolCallId": i.ToolCallID,
		"toolName":   i.ToolName,
		"input":      i.Details,
		"dynamic":    true,
	})

	e.send(map[string]any{
		"type":       "tool-approval-request",
		"approvalId": i.ToolCallID,
		"toolCallId": i.ToolCallID,
	})
	e.send(map[string]any{
		"type": "data-tool-interrupt",
		"id":   i.ToolCallID,
		"data": map[string]any{
			"toolCallId": i.ToolCallID,
			"toolName":   i.ToolName,
			"prompt":     i.Prompt,
			"details":    i.Details,
			"threadId":   i.ThreadID,
		},
	})

	e.send(map[string]any{"type": "finish"})
	_ = e.sink.SendRaw("[DONE]")
}

// ── metadata ─────────────────────────────────────────────────────────────────

// Metadata emits a message-metadata chunk, omitting zero/empty fields.
func (e *Encoder) Metadata(model, agentID string, durationMs int64) {
	md := map[string]any{}
	if model != "" {
		md["model"] = model
	}
	if agentID != "" {
		md["agentId"] = agentID
	}
	if durationMs != 0 {
		md["durationMs"] = durationMs
	}
	e.send(map[string]any{"type": "message-metadata", "messageMetadata": md})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// parseJSON parses s as JSON, falling back to the raw string on error.
func parseJSON(s string) any {
	if s == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	return v
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
