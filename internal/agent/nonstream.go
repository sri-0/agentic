package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agentic/internal/chat"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/genai"
)

// NonStreamAgentRun executes the agent and returns a standard ChatCompletion JSON response.
// If saver is non-nil, user and assistant messages are persisted to the thread.
func NonStreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID, userID string, messages []types.ChatMessage, saver *chat.MessageSaver, logger zerolog.Logger) {
	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])

	runLog := logger.With().Str("thread_id", threadID).Str("agent_id", core.AgentID).Logger()
	runLog.Info().Int("messages", len(messages)).Msg("non-stream: request received")

	if err := core.SessionManager.GetOrCreate(ctx, threadID); err != nil {
		runLog.Error().Err(err).Msg("non-stream: session create failed")
		http.Error(w, `{"error":"failed to create session"}`, http.StatusInternalServerError)
		return
	}
	runLog.Info().Msg("non-stream: session ready")

	lastMsg := messages[len(messages)-1]
	userContent := genai.NewContentFromText(lastMsg.Content, genai.RoleUser)

	// Persist user message
	if saver != nil {
		saver.SaveUserMessage(ctx, threadID, userID, lastMsg.Content)
	}

	var textContent string
	var finishReason string = "stop"
	startTime := time.Now()
	eventCount := 0
	toolCallCount := 0
	lastAuthor := ""

	runLog.Info().Msg("non-stream: runner started")

	for event, err := range core.Runner.Run(ctx, "default", threadID, userContent, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if err != nil {
			runLog.Error().Err(err).Dur("elapsed", time.Since(startTime)).Msg("non-stream: runner error")
			http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
			return
		}

		if event.Content == nil {
			continue
		}

		eventCount++
		author := event.Author

		// Log agent transitions
		if author != lastAuthor && author != "" {
			runLog.Info().Str("sub_agent", author).Int("event", eventCount).Msg("non-stream: agent active")
			lastAuthor = author
		}

		// Collect text from the output agent (or all agents if flat)
		isOutput := core.OutputAgent == "" || author == core.OutputAgent

		for _, part := range event.Content.Parts {
			if part.Text != "" && isOutput {
				textContent += part.Text
			}
			if fc := part.FunctionCall; fc != nil {
				toolCallCount++
				runLog.Info().Str("sub_agent", author).Str("tool", fc.Name).Str("call_id", fc.ID).Int("tool_call_num", toolCallCount).Dur("elapsed", time.Since(startTime)).Msg("non-stream: tool call")
			}
			if fr := part.FunctionResponse; fr != nil {
				runLog.Info().Str("sub_agent", author).Str("tool", fr.Name).Str("call_id", fr.ID).Dur("elapsed", time.Since(startTime)).Msg("non-stream: tool result")
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

	// Persist assistant message
	if saver != nil {
		saver.SaveAssistantMessage(ctx, threadID, textContent, core.AgentID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	runLog.Info().
		Int("events", eventCount).
		Int("tool_calls", toolCallCount).
		Int("output_chars", len(textContent)).
		Dur("elapsed", time.Since(startTime)).
		Msg("non-stream: agent run complete")
}
