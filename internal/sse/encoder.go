package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	openai "github.com/openai/openai-go/v3"
)

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

// ChunkBuilder builds OpenAI-compatible SSE chunks using openai-go types.
type ChunkBuilder struct {
	RequestID string
	Model     string
}

func NewChunkBuilder(requestID, model string) *ChunkBuilder {
	return &ChunkBuilder{RequestID: requestID, Model: model}
}

func (b *ChunkBuilder) base(delta openai.ChatCompletionChunkChoiceDelta, finishReason string) openai.ChatCompletionChunk {
	return openai.ChatCompletionChunk{
		ID:      b.RequestID,
		Created: time.Now().Unix(),
		Model:   b.Model,
		// Object zero value auto-marshals to "chat.completion.chunk"
		Choices: []openai.ChatCompletionChunkChoice{
			{Index: 0, Delta: delta, FinishReason: finishReason},
		},
	}
}

func (b *ChunkBuilder) TextDelta(content string) openai.ChatCompletionChunk {
	return b.base(openai.ChatCompletionChunkChoiceDelta{Content: content}, "")
}

func (b *ChunkBuilder) ToolCallDelta(index int64, id, name, args string) openai.ChatCompletionChunk {
	return b.base(openai.ChatCompletionChunkChoiceDelta{
		ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
			{
				Index:    index,
				ID:       id,
				Type:     "function",
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: name, Arguments: args},
			},
		},
	}, "")
}

func (b *ChunkBuilder) ToolCallsDelta(calls []openai.ChatCompletionChunkChoiceDeltaToolCall) openai.ChatCompletionChunk {
	return b.base(openai.ChatCompletionChunkChoiceDelta{ToolCalls: calls}, "")
}

func (b *ChunkBuilder) Finish(reason string) openai.ChatCompletionChunk {
	return b.base(openai.ChatCompletionChunkChoiceDelta{}, reason)
}
