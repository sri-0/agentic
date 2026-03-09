package types

import openai "github.com/openai/openai-go/v3"

// ChatCompletionRequest extends the OpenAI chat completion with thread tracking.
type ChatCompletionRequest struct {
	Model    string                          `json:"model"`
	Messages []openai.ChatCompletionMessage  `json:"messages"`
	Stream   *bool                           `json:"stream,omitempty"`
	ThreadID string                          `json:"thread_id,omitempty"`
}

// ResumeRequest is the payload for POST /v1/agent/resume.
type ResumeRequest struct {
	ThreadID string `json:"thread_id"`
	Action   string `json:"action"` // "approved" | "denied" | "skipped"
}

// ChatCompletionChunk extends the OpenAI streaming chunk with thread tracking.
type ChatCompletionChunk struct {
	openai.ChatCompletionChunk
	ThreadID string `json:"thread_id,omitempty"`
}
