package types

// ToolSummaryEvent is emitted as an SSE event during agent streaming
// after a batch of tool calls completes, providing a human-readable label.
type ToolSummaryEvent struct {
	ToolSummary struct {
		Label    string `json:"label"`
		Step     int    `json:"step"`
		ThreadID string `json:"thread_id"`
	} `json:"tool_summary"`
}
