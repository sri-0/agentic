package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToOpenAIRequest converts an Anthropic Messages API request to an OpenAI
// Chat Completions request body (as raw JSON bytes).
func ToOpenAIRequest(req *Request) ([]byte, error) {
	out := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	// Convert messages
	var messages []map[string]any

	// System prompt → OpenAI system message
	if len(req.System) > 0 {
		sysText := extractSystemText(req.System)
		if sysText != "" {
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": sysText,
			})
		}
	}

	for _, msg := range req.Messages {
		converted, err := convertMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("converting message: %w", err)
		}
		messages = append(messages, converted...)
	}
	out["messages"] = messages

	// Convert tools
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  json.RawMessage(t.InputSchema),
				},
			})
		}
		out["tools"] = tools
	}

	if len(req.ToolChoice) > 0 {
		out["tool_choice"] = convertToolChoice(req.ToolChoice)
	}

	return json.Marshal(out)
}

// extractSystemText handles system as either a plain string or []ContentBlock.
func extractSystemText(raw json.RawMessage) string {
	// Try plain string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try array of content blocks
	var blocks []ContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// convertMessage converts a single Anthropic message to one or more OpenAI messages.
// tool_result blocks become separate tool-role messages.
func convertMessage(msg RequestMessage) ([]map[string]any, error) {
	// Try simple string content
	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		return []map[string]any{{
			"role":    msg.Role,
			"content": text,
		}}, nil
	}

	// Array of content blocks
	var blocks []ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid content: %w", err)
	}

	// Separate tool_result blocks (they become tool-role messages in OpenAI)
	// and tool_use blocks (they become tool_calls on an assistant message)
	var textParts []map[string]any
	var toolCalls []map[string]any
	var results []map[string]any

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, map[string]any{
				"type": "text",
				"text": b.Text,
			})

		case "image":
			if b.Source != nil {
				textParts = append(textParts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data),
					},
				})
			}

		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": string(b.Input),
				},
			})

		case "tool_result":
			content := extractToolResultContent(b)
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      content,
			})
		}
	}

	var out []map[string]any

	// Build the main message
	if msg.Role == "assistant" && len(toolCalls) > 0 {
		m := map[string]any{
			"role":       "assistant",
			"tool_calls": toolCalls,
		}
		if len(textParts) == 1 {
			m["content"] = textParts[0]["text"]
		} else if len(textParts) > 1 {
			m["content"] = textParts
		}
		out = append(out, m)
	} else if len(textParts) == 1 {
		out = append(out, map[string]any{
			"role":    msg.Role,
			"content": textParts[0]["text"],
		})
	} else if len(textParts) > 1 {
		out = append(out, map[string]any{
			"role":    msg.Role,
			"content": textParts,
		})
	}

	// tool_result blocks become separate tool messages
	out = append(out, results...)

	return out, nil
}

func extractToolResultContent(b ContentBlock) string {
	if len(b.Content2) == 0 {
		return ""
	}

	// Try plain string
	var s string
	if json.Unmarshal(b.Content2, &s) == nil {
		return s
	}

	// Try array of content blocks
	var blocks []ContentBlock
	if json.Unmarshal(b.Content2, &blocks) == nil {
		var parts []string
		for _, bb := range blocks {
			if bb.Type == "text" {
				parts = append(parts, bb.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return string(b.Content2)
}

func convertToolChoice(raw json.RawMessage) any {
	// Anthropic tool_choice can be:
	// {"type": "auto"} → "auto"
	// {"type": "any"} → "required"
	// {"type": "tool", "name": "..."} → {"type": "function", "function": {"name": "..."}}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return "auto"
	}

	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}
	default:
		return "auto"
	}
}
