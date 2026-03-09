package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// WriteEvent writes a single Anthropic SSE event (event: + data:).
func WriteEvent(w http.ResponseWriter, eventType string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// StreamConverter reads an OpenAI SSE stream and writes Anthropic SSE events.
type StreamConverter struct {
	Model     string
	RequestID string
}

// Convert reads the OpenAI SSE stream from r and writes Anthropic SSE to w.
func (sc *StreamConverter) Convert(w http.ResponseWriter, r io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// State tracking
	var (
		contentStarted   bool
		blockIndex       int
		toolCallStarted  = map[int]bool{}    // openai tool index → started
		toolBlockIndex   = map[int]int{}     // openai tool index → anthropic block index
		totalOutputTokens int
	)

	// Send message_start
	WriteEvent(w, "message_start", MessageStartEvent{
		Type: "message_start",
		Message: Response{
			ID:         sc.RequestID,
			Type:       "message",
			Role:       "assistant",
			Content:    []ResponseContent{},
			Model:      sc.Model,
			StopReason: nil,
			Usage:      Usage{InputTokens: 0, OutputTokens: 0},
		},
	})

	// Send ping
	WriteEvent(w, "ping", PingEvent{Type: "ping"})

	scanner := bufio.NewScanner(r)
	// Increase buffer for large responses
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		if data == "[DONE]" {
			break
		}

		var chunk openAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) == 0 {
			// Could be a usage-only chunk
			if chunk.Usage != nil {
				totalOutputTokens = chunk.Usage.CompletionTokens
			}
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		// Handle text content
		if delta.Content != "" {
			if !contentStarted {
				WriteEvent(w, "content_block_start", ContentBlockStartEvent{
					Type:         "content_block_start",
					Index:        blockIndex,
					ContentBlock: ResponseContent{Type: "text", Text: ""},
				})
				contentStarted = true
			}

			WriteEvent(w, "content_block_delta", ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: blockIndex,
				Delta: Delta{Type: "text_delta", Text: delta.Content},
			})
			totalOutputTokens++
		}

		// Handle tool calls
		for _, tc := range delta.ToolCalls {
			if !toolCallStarted[tc.Index] {
				// Close text block if open
				if contentStarted && blockIndex == 0 {
					WriteEvent(w, "content_block_stop", ContentBlockStopEvent{
						Type:  "content_block_stop",
						Index: blockIndex,
					})
					blockIndex++
					contentStarted = false
				}

				toolBlockIndex[tc.Index] = blockIndex
				toolCallStarted[tc.Index] = true

				WriteEvent(w, "content_block_start", ContentBlockStartEvent{
					Type:  "content_block_start",
					Index: blockIndex,
					ContentBlock: ResponseContent{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: json.RawMessage("{}"),
					},
				})
				blockIndex++
			}

			if tc.Function.Arguments != "" {
				WriteEvent(w, "content_block_delta", ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: toolBlockIndex[tc.Index],
					Delta: Delta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments},
				})
			}
		}

		// Handle finish
		if choice.FinishReason != nil {
			// Close any open content block
			if contentStarted {
				WriteEvent(w, "content_block_stop", ContentBlockStopEvent{
					Type:  "content_block_stop",
					Index: 0,
				})
			}

			// Close any open tool blocks
			for oaiIdx, started := range toolCallStarted {
				if started {
					WriteEvent(w, "content_block_stop", ContentBlockStopEvent{
						Type:  "content_block_stop",
						Index: toolBlockIndex[oaiIdx],
					})
				}
			}

			stopReason := mapFinishReason(*choice.FinishReason)
			WriteEvent(w, "message_delta", MessageDeltaEvent{
				Type: "message_delta",
				Delta: Delta{
					Type:       "message_delta",
					StopReason: &stopReason,
				},
				Usage: &UsageDelta{OutputTokens: totalOutputTokens},
			})

			WriteEvent(w, "message_stop", MessageStopEvent{Type: "message_stop"})
		}
	}
}

// ConvertNonStreaming converts a full OpenAI response body to an Anthropic Message response.
func ConvertNonStreaming(model string, requestID string, body []byte) ([]byte, error) {
	var oaiResp openAIResponse
	if err := json.Unmarshal(body, &oaiResp); err != nil {
		return nil, fmt.Errorf("parsing openai response: %w", err)
	}

	resp := Response{
		ID:    requestID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
		Usage: Usage{},
	}

	if oaiResp.Usage != nil {
		resp.Usage.InputTokens = oaiResp.Usage.PromptTokens
		resp.Usage.OutputTokens = oaiResp.Usage.CompletionTokens
	}

	if len(oaiResp.Choices) > 0 {
		choice := oaiResp.Choices[0]

		if choice.FinishReason != nil {
			sr := mapFinishReason(*choice.FinishReason)
			resp.StopReason = &sr
		}

		msg := choice.Message

		// Text content
		if msg.Content != "" {
			resp.Content = append(resp.Content, ResponseContent{
				Type: "text",
				Text: msg.Content,
			})
		}

		// Tool calls
		for _, tc := range msg.ToolCalls {
			resp.Content = append(resp.Content, ResponseContent{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			})
		}
	}

	return json.Marshal(resp)
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// ── OpenAI response types for parsing ──────────────────────────────────────

type openAIChunk struct {
	ID      string         `json:"id"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Delta        openAIDelta   `json:"delta"`
	Message      openAIMessage `json:"message"`
	FinishReason *string       `json:"finish_reason"`
}

type openAIDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function openAIFuncCall   `json:"function"`
}

type openAIFuncCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
