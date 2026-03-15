package hitl

// PendingInterrupt stores the minimal info needed to map a thread to its
// ADK confirmation call ID so the resume endpoint can construct the
// FunctionResponse to send back to the runner.
type PendingInterrupt struct {
	AgentID            string         `json:"agent_id"`
	ConfirmationCallID string         `json:"confirmation_call_id"`
	ToolCallID         string         `json:"tool_call_id"`
	ToolName           string         `json:"tool_name"`
	Prompt             string         `json:"prompt"`
	Details            map[string]any `json:"details"`
}

// Store persists pending HITL interrupts keyed by thread ID.
type Store interface {
	Set(threadID string, p *PendingInterrupt) error
	Get(threadID string) (*PendingInterrupt, error)
	Clear(threadID string) error
}
