package types

import (
	"encoding/json"

	openai "github.com/openai/openai-go/v3"
)

// ChatCompletionRequest for parsing the fields we care about.
type ChatCompletionRequest struct {
	Model        string        `json:"model" jsonschema:"required" jsonschema_description:"Model ID to use"`
	Messages     []ChatMessage `json:"messages" jsonschema:"required" jsonschema_description:"Conversation messages"`
	Stream       *bool         `json:"stream,omitempty" jsonschema_description:"Stream response via SSE (default: true)"`
	ThreadID     string        `json:"thread_id,omitempty" jsonschema_description:"Thread ID for conversation persistence"`
	UseRAG       bool          `json:"use_rag,omitempty" jsonschema_description:"Augment with knowledge base context before LLM call"`
	PromptID     string        `json:"prompt_id,omitempty" jsonschema_description:"Prompt template ID to apply"`
	AgentID      string        `json:"agent_id,omitempty" jsonschema_description:"Agent ID to route this request to"`
	AgentIDCamel string        `json:"agentId,omitempty" jsonschema:"-"`
}

// RouteAgentID returns the explicit agent selector, accepting both snake_case
// and camelCase payloads used by OpenAI-compatible clients and web UIs.
func (r ChatCompletionRequest) RouteAgentID() string {
	if r.AgentID != "" {
		return r.AgentID
	}
	return r.AgentIDCamel
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

// ReasoningChunk is an OpenAI-compatible streaming chunk that carries a
// `reasoning` field on the delta (OpenRouter convention). The OpenAI SDK's
// delta struct has no reasoning field, so we model the chunk explicitly.
// Field names and shape match the standard chat.completion.chunk wire format.
type ReasoningChunk struct {
	ID       string                 `json:"id"`
	Object   string                 `json:"object"`
	Created  int64                  `json:"created"`
	Model    string                 `json:"model"`
	Choices  []ReasoningChunkChoice `json:"choices"`
	ThreadID string                 `json:"thread_id,omitempty"`
}

type ReasoningChunkChoice struct {
	Index        int            `json:"index"`
	Delta        ReasoningDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type ReasoningDelta struct {
	Reasoning string `json:"reasoning"`
}
