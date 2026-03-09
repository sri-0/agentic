package types

import openai "github.com/openai/openai-go/v3"

// ChatCompletionRequest for parsing the fields we care about.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   *bool         `json:"stream,omitempty"`
	ThreadID string        `json:"thread_id,omitempty"`
}

// ChatMessage is a minimal message type for extracting role/content.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
