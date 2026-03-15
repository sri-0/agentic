package types

import (
	"encoding/json"

	openai "github.com/openai/openai-go/v3"
)

// ChatCompletionRequest for parsing the fields we care about.
type ChatCompletionRequest struct {
	Model    string        `json:"model" jsonschema:"required" jsonschema_description:"Model ID to use"`
	Messages []ChatMessage `json:"messages" jsonschema:"required" jsonschema_description:"Conversation messages"`
	Stream   *bool         `json:"stream,omitempty" jsonschema_description:"Stream response via SSE (default: true)"`
	ThreadID string        `json:"thread_id,omitempty" jsonschema_description:"Thread ID for conversation persistence"`
	UseRAG   bool          `json:"use_rag,omitempty" jsonschema_description:"Augment with knowledge base context before LLM call"`
	PromptID string        `json:"prompt_id,omitempty" jsonschema_description:"Prompt template ID to apply"`
}

// ChatMessage is a minimal message type for extracting role/content.
type ChatMessage struct {
	Role    string `json:"role" jsonschema:"required" jsonschema_description:"Message role: system, user, assistant, tool"`
	Content string `json:"content" jsonschema:"required" jsonschema_description:"Message content"`
}

type ChatCompletionResponse struct {
	ID      string          `json:"id" jsonschema_description:"Response ID"`
	Object  string          `json:"object" jsonschema_description:"Always 'chat.completion'"`
	Model   string          `json:"model" jsonschema_description:"Model used"`
	Choices json.RawMessage `json:"choices" jsonschema_description:"Completion choices"`
	Usage   json.RawMessage `json:"usage,omitempty" jsonschema_description:"Token usage statistics"`
}

// ResumeRequest is the payload for POST /v1/agent/resume.
type ResumeRequest struct {
	ThreadID string `json:"thread_id" jsonschema:"required" jsonschema_description:"Thread ID of the paused agent"`
	Action   string `json:"action" jsonschema:"required" jsonschema_description:"One of: approved, denied, skipped"`
}

// ChatCompletionChunk extends the OpenAI streaming chunk with thread tracking.
type ChatCompletionChunk struct {
	openai.ChatCompletionChunk
	ThreadID string `json:"thread_id,omitempty"`
}
