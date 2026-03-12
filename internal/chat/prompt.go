package chat

import (
	"context"
	"encoding/json"

	"agentic/internal/types"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

// ApplyPromptTemplate fetches a prompt by ID from the prompts index and inserts
// it as a system message after any existing system messages. No-op if osClient
// is nil or promptID is empty.
func ApplyPromptTemplate(ctx context.Context, osClient *opensearch.Client, promptID string, messages []types.ChatMessage, logger zerolog.Logger) []types.ChatMessage {
	if osClient == nil || promptID == "" {
		return messages
	}

	hit, err := osClient.GetDocument(ctx, opensearch.IndexPrompts, promptID)
	if err != nil {
		logger.Warn().Err(err).Str("prompt_id", promptID).Msg("failed to fetch prompt template, proceeding without it")
		return messages
	}

	var prompt struct {
		Template string `json:"template"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(hit.Source, &prompt); err != nil {
		logger.Warn().Err(err).Str("prompt_id", promptID).Msg("failed to parse prompt template")
		return messages
	}

	if prompt.Template == "" {
		return messages
	}

	// Insert after existing system messages so it augments rather than replaces.
	insertIdx := 0
	for i, m := range messages {
		if m.Role == "system" {
			insertIdx = i + 1
		} else {
			break
		}
	}

	out := make([]types.ChatMessage, 0, len(messages)+1)
	out = append(out, messages[:insertIdx]...)
	out = append(out, types.ChatMessage{Role: "system", Content: prompt.Template})
	out = append(out, messages[insertIdx:]...)

	logger.Debug().Str("prompt_id", promptID).Str("prompt_name", prompt.Name).Msg("prompt template applied")
	return out
}
