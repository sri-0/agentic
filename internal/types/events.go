package types

// AgentProgressEvent is a custom SSE extension for progress updates.
type AgentProgressEvent struct {
	AgentProgress struct {
		Phase   string `json:"phase"`
		Message string `json:"message"`
	} `json:"agent_progress"`
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
