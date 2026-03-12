package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/genai"
)

// NonStreamAgentRun executes the agent and returns a standard ChatCompletion JSON response.
func NonStreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, messages []types.ChatMessage, logger zerolog.Logger) {
	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])

	logger.Info().Str("thread_id", threadID).Str("agent_id", core.AgentID).Int("messages", len(messages)).Msg("non-stream start")

	if err := core.SessionManager.GetOrCreate(ctx, threadID); err != nil {
		logger.Error().Err(err).Str("thread_id", threadID).Msg("session create failed")
		http.Error(w, `{"error":"failed to create session"}`, http.StatusInternalServerError)
		return
	}

	lastMsg := messages[len(messages)-1]
	userContent := genai.NewContentFromText(lastMsg.Content, genai.RoleUser)

	var textContent string
	var finishReason string = "stop"

	for event, err := range core.Runner.Run(ctx, "default", threadID, userContent, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if err != nil {
			logger.Error().Err(err).Msg("runner error")
			http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
			return
		}

		if event.Content == nil {
			continue
		}

		// Collect text from the output agent (or all agents if flat)
		author := event.Author
		isOutput := core.OutputAgent == "" || author == core.OutputAgent

		if isOutput {
			for _, part := range event.Content.Parts {
				if part.Text != "" {
					textContent += part.Text
				}
			}
		}
	}

	resp := map[string]any{
		"id":      requestID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   core.AgentID,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": textContent,
				},
				"finish_reason": finishReason,
			},
		},
		"thread_id": threadID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	logger.Info().Str("thread_id", threadID).Int("content_len", len(textContent)).Msg("non-stream complete")
}
