package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agentic/internal/types"

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

// ChunkBuilder builds extended OpenAI SSE chunks with thread tracking.
type ChunkBuilder struct {
	RequestID string
	Model     string
	ThreadID  string
}

func NewChunkBuilder(requestID, model, threadID string) *ChunkBuilder {
	return &ChunkBuilder{RequestID: requestID, Model: model, ThreadID: threadID}
}

func (b *ChunkBuilder) base(delta openai.ChatCompletionChunkChoiceDelta, finishReason string) types.ChatCompletionChunk {
	return types.ChatCompletionChunk{
		ChatCompletionChunk: openai.ChatCompletionChunk{
			ID:      b.RequestID,
			Created: time.Now().Unix(),
			Model:   b.Model,
			Choices: []openai.ChatCompletionChunkChoice{
				{Index: 0, Delta: delta, FinishReason: finishReason},
			},
		},
		ThreadID: b.ThreadID,
	}
}

func (b *ChunkBuilder) TextDelta(content string) types.ChatCompletionChunk {
	return b.base(openai.ChatCompletionChunkChoiceDelta{Content: content}, "")
}

// ReasoningDelta mirrors TextDelta but writes the OpenRouter-compatible
// `reasoning` field on the delta instead of `content`. Used to surface
// the output agent's thinking tokens to the frontend, which maps
// choices[0].delta.reasoning to a native reasoning part.
func (b *ChunkBuilder) ReasoningDelta(text string) types.ReasoningChunk {
	return types.ReasoningChunk{
		ID:      b.RequestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   b.Model,
		Choices: []types.ReasoningChunkChoice{
			{Index: 0, Delta: types.ReasoningDelta{Reasoning: text}},
		},
		ThreadID: b.ThreadID,
	}
}

// FinishWithUsage emits a terminal chunk carrying token usage statistics,
// matching the OpenAI stream_options:{include_usage:true} final chunk shape.
func (b *ChunkBuilder) FinishWithUsage(reason string, promptTokens, completionTokens, totalTokens int) types.ChatCompletionChunk {
	chunk := b.base(openai.ChatCompletionChunkChoiceDelta{}, reason)
	chunk.Usage = openai.CompletionUsage{
		PromptTokens:     int64(promptTokens),
		CompletionTokens: int64(completionTokens),
		TotalTokens:      int64(totalTokens),
	}
	return chunk
}

func (b *ChunkBuilder) ToolCallDelta(index int64, id, name, args string) types.ChatCompletionChunk {
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

func (b *ChunkBuilder) ToolCallsDelta(calls []openai.ChatCompletionChunkChoiceDeltaToolCall) types.ChatCompletionChunk {
	return b.base(openai.ChatCompletionChunkChoiceDelta{ToolCalls: calls}, "")
}

func (b *ChunkBuilder) Finish(reason string) types.ChatCompletionChunk {
	return b.base(openai.ChatCompletionChunkChoiceDelta{}, reason)
}
