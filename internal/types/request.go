package types

type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   *bool         `json:"stream,omitempty"`
	ThreadID string        `json:"thread_id,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ResumeRequest struct {
	ThreadID string `json:"thread_id"`
	Action   string `json:"action"` // "approved" | "denied" | "skipped"
}
