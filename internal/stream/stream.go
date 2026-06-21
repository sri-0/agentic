// Package stream defines a pluggable encoder abstraction over the adk-go event
// loop. The agent event-processing loop (internal/agent.streamEvents) calls the
// semantic Encoder methods; concrete encoders translate those into a wire
// format. Two encoders exist: `openai` (the OpenAI chat.completion + ag_ui SSE,
// the default) and `aisdk` (the Vercel AI SDK v6 UI Message Stream). The format
// is chosen per-request via the `?format=` query param.
//
// Sink abstracts the transport (SSE today; a WebSocket sink is a drop-in swap),
// so encoders never touch http.ResponseWriter directly.
package stream

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Sink is the transport an Encoder writes to.
type Sink interface {
	// Send marshals v to JSON and delivers it as one stream event.
	Send(v any) error
	// SendRaw delivers a pre-encoded payload (used for the `[DONE]` sentinel).
	SendRaw(payload string) error
	// Flush pushes buffered bytes to the client.
	Flush()
}

// SSESink writes Vercel-style Server-Sent Events: `data: {json}\n\n`.
type SSESink struct {
	w http.ResponseWriter
	f http.Flusher
}

// NewSSESink wraps an http.ResponseWriter. Headers are the caller's job.
func NewSSESink(w http.ResponseWriter) *SSESink {
	f, _ := w.(http.Flusher)
	return &SSESink{w: w, f: f}
}

func (s *SSESink) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.Flush()
	return nil
}

func (s *SSESink) SendRaw(payload string) error {
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	s.Flush()
	return nil
}

func (s *SSESink) Flush() {
	if s.f != nil {
		s.f.Flush()
	}
}

// ── Shared value types ───────────────────────────────────────────────────────

// Usage holds token counts captured from an adk run.
type Usage struct {
	Prompt        int
	Completion    int
	Total         int
	ContextWindow int
}

// Bucket is one row of the context-usage breakdown.
type Bucket struct {
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
}

// Task is one entry of the live task board, already mapped to UI status values
// (pending | in_progress | completed | cancelled). Agent is the owning
// sub-agent (worker) name, empty for todowrite-sourced lists.
type Task struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
	Agent    string `json:"agent,omitempty"`
}

// Interrupt carries a HITL tool-confirmation request.
type Interrupt struct {
	ToolCallID string
	ToolName   string
	Prompt     string
	Details    any
	ThreadID   string
}

// ── Encoder ──────────────────────────────────────────────────────────────────

// Encoder turns semantic stream events into a concrete wire format. One Encoder
// instance serves one request/response. Methods are called in stream order from
// the single event loop, so implementations need not be safe for concurrent use.
type Encoder interface {
	// Lifecycle.
	RunStarted()
	// RunFinished emits the terminal usage, finish, and [DONE] framing.
	RunFinished(u Usage)

	// Run-level (un-attributed) progress, e.g. planning / fatal error.
	Progress(phase, message string)

	// Output (main-thread) agent.
	Text(delta string)
	Reasoning(delta string)
	ToolCall(index int64, id, name, argsJSON string)
	ToolResult(id, name string, response any)

	// Sub-agents (attributed by agent + step).
	AgentStart(agent string, step int)
	AgentDone(agent string, step int, durationMs int64)
	AgentProgress(phase, message, agent string, step int)
	AgentText(agent string, step int, delta string)
	AgentTextDone(agent string, step int)
	AgentReasoning(agent string, step int, delta string)
	AgentToolCall(agent string, step int, name, id, argsJSON string)
	AgentToolResult(agent string, step int, name, id, content string)

	// Data parts.
	Artifact(value map[string]any)
	TaskList(tasks []Task)
	Usage(u Usage, breakdown []Bucket)

	// HITL — the encoder owns the full pause sequence (tool call surfaced,
	// interrupt advertised, stream closed). After this the loop returns.
	ToolInterrupt(i Interrupt)

	// Message metadata (model / agent id / elapsed). May be a no-op for formats
	// that have no metadata channel.
	Metadata(model, agentID string, durationMs int64)
}

// Format identifies a wire format selectable via the `?format=` query param.
type Format string

const (
	FormatOpenAI Format = "openai"
	FormatAISDK  Format = "aisdk"
)

// ParseFormat maps a query-param value to a Format, defaulting to OpenAI.
func ParseFormat(s string) Format {
	switch s {
	case string(FormatAISDK), "ai-sdk", "ai_sdk", "vercel":
		return FormatAISDK
	default:
		return FormatOpenAI
	}
}
