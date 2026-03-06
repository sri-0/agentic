package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/proxy"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/genai"
)

func Chat(core *agent.Core, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "invalid request: %s"}`, err), http.StatusBadRequest)
			return
		}

		// Model-based routing: agent mode vs proxy
		if req.Model != core.Config.AgentModelName {
			proxy.Forward(w, r, core.Config.LLMBaseURL, core.Config.LLMAPIKey, req)
			return
		}

		// Agent mode
		threadID := req.ThreadID
		if threadID == "" {
			threadID = fmt.Sprintf("anon-%s", uuid.New().String()[:12])
		}

		// Ensure session exists
		if err := core.SessionManager.GetOrCreate(r.Context(), threadID); err != nil {
			logger.Error().Err(err).Str("thread_id", threadID).Msg("failed to create session")
			http.Error(w, `{"error": "failed to create session"}`, http.StatusInternalServerError)
			return
		}

		// Convert last user message to genai.Content
		var userMsg *genai.Content
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				userMsg = genai.NewContentFromText(req.Messages[i].Content, genai.RoleUser)
				break
			}
		}
		if userMsg == nil {
			http.Error(w, `{"error": "no user message found"}`, http.StatusBadRequest)
			return
		}

		logger.Info().Str("thread_id", threadID).Int("messages", len(req.Messages)).Msg("agent chat")
		agent.StreamAgentRun(r.Context(), w, core, threadID, userMsg, logger)
	}
}
