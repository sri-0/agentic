package anthropic

import "encoding/json"

// ── Request types (Anthropic → us) ─────────────────────────────────────────

type Request struct {
	Model         string           `json:"model"`
	Messages      []RequestMessage `json:"messages"`
	System        json.RawMessage  `json:"system,omitempty"` // string or []ContentBlock
	MaxTokens     int              `json:"max_tokens"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	TopK          *int             `json:"top_k,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Tools         []Tool           `json:"tools,omitempty"`
	ToolChoice    json.RawMessage  `json:"tool_choice,omitempty"`
	Metadata      json.RawMessage  `json:"metadata,omitempty"`
}

type RequestMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock
}

type ContentBlock struct {
	Type string `json:"type"`

	// text block
	Text string `json:"text,omitempty"`

	// image block
	Source *ImageSource `json:"source,omitempty"`

	// tool_use block
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result block
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content2  json.RawMessage `json:"content,omitempty"` // nested content for tool_result
	IsError   bool            `json:"is_error,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ── Response types (us → Anthropic client) ─────────────────────────────────

type Response struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"` // "message"
	Role         string               `json:"role"` // "assistant"
	Content      []ResponseContent    `json:"content"`
	Model        string               `json:"model"`
	StopReason   *string              `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        Usage                `json:"usage"`
}

type ResponseContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ── SSE event types ────────────────────────────────────────────────────────

type MessageStartEvent struct {
	Type    string   `json:"type"` // "message_start"
	Message Response `json:"message"`
}

type ContentBlockStartEvent struct {
	Type         string          `json:"type"` // "content_block_start"
	Index        int             `json:"index"`
	ContentBlock ResponseContent `json:"content_block"`
}

type ContentBlockDeltaEvent struct {
	Type  string `json:"type"` // "content_block_delta"
	Index int    `json:"index"`
	Delta Delta  `json:"delta"`
}

type Delta struct {
	Type        string  `json:"type"`
	Text        string  `json:"text,omitempty"`         // text_delta
	PartialJSON string  `json:"partial_json,omitempty"` // input_json_delta
	StopReason  *string `json:"stop_reason,omitempty"`  // message_delta
	StopSequence *string `json:"stop_sequence,omitempty"`
}

type ContentBlockStopEvent struct {
	Type  string `json:"type"` // "content_block_stop"
	Index int    `json:"index"`
}

type MessageDeltaEvent struct {
	Type  string     `json:"type"` // "message_delta"
	Delta Delta      `json:"delta"`
	Usage *UsageDelta `json:"usage,omitempty"`
}

type UsageDelta struct {
	OutputTokens int `json:"output_tokens"`
}

type MessageStopEvent struct {
	Type string `json:"type"` // "message_stop"
}

type PingEvent struct {
	Type string `json:"type"` // "ping"
}
