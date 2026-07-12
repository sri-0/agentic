// Package eventlog is the durable, per-session, sequence-numbered event log
// that decouples agent execution from the HTTP connection and powers
// exactly-once stream resume. It is a dependency-light leaf package: the typed
// AgentEvent union here is the source of truth; higher layers (the run
// coordinator, the SSE pump, history projection) consume it.
//
// Two interchangeable backends implement the EventLog port: an in-memory store
// (default, no external deps) and a Redis Streams store (production, swappable
// to Kafka later behind the same interface).
package eventlog

// EventType discriminates the AgentEvent union.
type EventType string

const (
	EvRunStatus      EventType = "run-status"      // queued|running|awaiting-input|done|error|cancelled
	EvTextDelta      EventType = "text-delta"      // output-agent assistant token
	EvReasoningDelta EventType = "reasoning-delta" // output-agent reasoning token
	EvToolCall       EventType = "tool-call"
	EvToolResult     EventType = "tool-result"
	EvAgentStep      EventType = "agent-step"  // sub-agent start/done (Kind: started|done)
	EvAgentDelta     EventType = "agent-delta" // sub-agent token (Kind: text|reasoning)
	EvTaskList       EventType = "task-list"   // full board snapshot
	EvArtifact       EventType = "artifact"
	EvUsage          EventType = "usage"
	EvQuestion       EventType = "question-asked"  // HITL / interactive question; run suspends
	EvHITLResolved   EventType = "hitl-resolved"   // a question/confirmation was answered
	EvProgress       EventType = "progress"        // run-level or sub-agent progress note
	EvMetadata       EventType = "metadata"        // model/agent id/elapsed
	EvHeartbeat      EventType = "heartbeat"       // keep-alive (Seq == -1, not persisted)
)

// Agent-delta / agent-step Kind values.
const (
	KindText      = "text"
	KindReasoning = "reasoning"
	KindStarted   = "started"
	KindDone      = "done"
	KindTextDone  = "text-done"
)

// hitl-resolved Kind values: how the question/confirmation was answered. Stamped
// on EvHITLResolved so the history projection can mark the interrupt part
// resolved with the right decision. Older events without a Kind default to
// approved (a resume implies the run continued).
const (
	KindApproved = "approved"
	KindDenied   = "denied"
)

// RunStatus values for EvRunStatus events.
const (
	StatusQueued        = "queued"
	StatusRunning       = "running"
	StatusAwaitingInput = "awaiting-input"
	StatusDone          = "done"
	StatusError         = "error"
	StatusCancelled     = "cancelled"
)

// AgentEvent is one durable, versioned event in a session's log. Exactly one of
// the payload fields is populated per Type; flat+omitempty keeps the JSON
// compact and OpenSearch-friendly.
type AgentEvent struct {
	V        int       `json:"v"`
	Type     EventType `json:"type"`
	Ts       int64     `json:"ts"` // unix ms

	// Attribution, resolved ONCE at write time so readers never need the Core.
	InvocationID string `json:"inv,omitempty"`
	Author       string `json:"author,omitempty"` // sub-agent / child session label ("" = output agent)
	SubagentType string `json:"subagent_type,omitempty"`
	SessionID    string `json:"session_id,omitempty"` // child session id for sub-agent events
	Step         int    `json:"step,omitempty"`
	IsOutput     bool   `json:"is_output,omitempty"`

	// Payloads (one per Type).
	Text     string           `json:"text,omitempty"`     // text/reasoning/agent deltas
	Kind     string           `json:"kind,omitempty"`     // agent-delta: text|reasoning|text-done; agent-step: started|done
	Duration int64            `json:"duration_ms,omitempty"`
	Tool     *ToolPayload     `json:"tool,omitempty"`
	Tasks    []TaskItem       `json:"tasks,omitempty"`
	Artifact map[string]any   `json:"artifact,omitempty"`
	Usage    *UsagePayload    `json:"usage,omitempty"`
	Question *QuestionPayload `json:"question,omitempty"`
	Status   string           `json:"status,omitempty"`
	Err      string           `json:"err,omitempty"`
	RunID    string           `json:"run_id,omitempty"` // stamped on run-status events to distinguish runs in one session log

	// Progress / metadata payloads.
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	Model   string `json:"model,omitempty"`
}

// ToolPayload carries a tool call or result.
type ToolPayload struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Args   any    `json:"args,omitempty"`
	Result any    `json:"result,omitempty"`
}

// TaskItem is one row of a task-list snapshot (mirrors the UI data-task-list shape).
type TaskItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
	Agent    string `json:"agent,omitempty"`
}

// UsagePayload carries token accounting.
type UsagePayload struct {
	PromptTokens     int          `json:"prompt_tokens"`
	CompletionTokens int          `json:"completion_tokens"`
	TotalTokens      int          `json:"total_tokens"`
	ContextWindow    int          `json:"context_window,omitempty"`
	ContextUsed      int          `json:"context_used,omitempty"`
	Breakdown        []UsageBucket `json:"breakdown,omitempty"`
}

// UsageBucket is one labelled slice of token usage (mirrors stream.Bucket).
type UsageBucket struct {
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
}

// QuestionPayload carries an interactive question / HITL confirmation request.
type QuestionPayload struct {
	RequestID          string         `json:"request_id"`
	ToolCallID         string         `json:"tool_call_id,omitempty"`
	ToolName           string         `json:"tool_name,omitempty"`
	ConfirmationCallID string         `json:"confirmation_call_id,omitempty"`
	Prompt             string         `json:"prompt,omitempty"`
	Questions          []any          `json:"questions,omitempty"` // question-tool schema (Phase 05)
	Details            map[string]any `json:"details,omitempty"`
}

// IsTerminal reports whether ev is a run-status event ending the run.
func (ev AgentEvent) IsTerminal() bool {
	return ev.Type == EvRunStatus &&
		(ev.Status == StatusDone || ev.Status == StatusError ||
			ev.Status == StatusCancelled || ev.Status == StatusAwaitingInput)
}
