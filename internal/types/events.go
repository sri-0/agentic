package types

// AgentProgressEvent is a custom SSE extension for agent lifecycle updates.
// Emitted when agents start/finish, with step tracking for multi-agent workflows.
type AgentProgressEvent struct {
	AgentProgress struct {
		Phase   string `json:"phase"`
		Message string `json:"message"`
		Agent   string `json:"agent,omitempty"`
		Step    int    `json:"step,omitempty"`
	} `json:"agent_progress"`
}

// AgentEventEvent is a custom SSE extension for sub-agent streaming output.
// Used for intermediate agent text that should render separately from the final response.
type AgentEventEvent struct {
	AgentEvent struct {
		Agent    string         `json:"agent"`
		Type     string         `json:"type"` // text_delta, text_done, step_start, step_done
		Content  string         `json:"content,omitempty"`
		Step     int            `json:"step,omitempty"`
		Metadata map[string]any `json:"metadata,omitempty"`
	} `json:"agent_event"`
}

// ToolResultEvent is a custom SSE extension for tool results.
type ToolResultEvent struct {
	ToolResult struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Result     any    `json:"result"`
	} `json:"tool_result"`
}

// ToolInterruptEvent is a custom SSE extension for HITL interrupts.
type ToolInterruptEvent struct {
	ToolInterrupt struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Prompt     string `json:"prompt"`
		Details    any    `json:"details"`
		ThreadID   string `json:"thread_id"`
	} `json:"tool_interrupt"`
}
