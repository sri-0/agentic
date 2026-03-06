package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ChatCompletionChunk is an OpenAI-compatible SSE chunk.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model,omitempty"`
	Choices []ChunkChoice `json:"choices"`
}

type ChunkChoice struct {
	Index        int         `json:"index"`
	Delta        ChunkDelta  `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type ChunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

type ToolCallDelta struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function *ToolCallFunctionDelta `json:"function,omitempty"`
}

type ToolCallFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Encode marshals data to SSE format: "data: {json}\n\n"
func Encode(data any) ([]byte, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("data: %s\n\n", b)), nil
}

// EncodeDone returns the SSE done sentinel.
func EncodeDone() []byte {
	return []byte("data: [DONE]\n\n")
}

// WriteSSE writes an arbitrary data object as an SSE event.
func WriteSSE(w http.ResponseWriter, data any) error {
	b, err := Encode(data)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	if err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// WriteDone writes the SSE done sentinel.
func WriteDone(w http.ResponseWriter) error {
	_, err := w.Write(EncodeDone())
	if err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// ChunkBuilder builds OpenAI-compatible SSE chunks.
type ChunkBuilder struct {
	RequestID string
	Model     string
}

func NewChunkBuilder(requestID, model string) *ChunkBuilder {
	return &ChunkBuilder{RequestID: requestID, Model: model}
}

func (b *ChunkBuilder) base(delta ChunkDelta, finishReason *string) ChatCompletionChunk {
	return ChatCompletionChunk{
		ID:      b.RequestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   b.Model,
		Choices: []ChunkChoice{
			{Index: 0, Delta: delta, FinishReason: finishReason},
		},
	}
}

func (b *ChunkBuilder) TextDelta(content string) ChatCompletionChunk {
	return b.base(ChunkDelta{Content: content}, nil)
}

func (b *ChunkBuilder) ReasoningDelta(content string) ChatCompletionChunk {
	return b.base(ChunkDelta{ReasoningContent: content}, nil)
}

func (b *ChunkBuilder) ToolCallDelta(index int, id, name, args string) ChatCompletionChunk {
	delta := ChunkDelta{
		ToolCalls: []ToolCallDelta{
			{
				Index: index,
				ID:    id,
				Type:  "function",
				Function: &ToolCallFunctionDelta{
					Name:      name,
					Arguments: args,
				},
			},
		},
	}
	return b.base(delta, nil)
}

func (b *ChunkBuilder) ToolCallsDelta(calls []ToolCallDelta) ChatCompletionChunk {
	return b.base(ChunkDelta{ToolCalls: calls}, nil)
}

func (b *ChunkBuilder) Finish(reason string) ChatCompletionChunk {
	return b.base(ChunkDelta{}, &reason)
}
