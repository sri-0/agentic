package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/config"
	"agentic/internal/proxy"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

func Chat(registry *agent.Registry, cfg *config.Config, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error": "failed to read request body"}`, http.StatusBadRequest)
			return
		}

		var req types.ChatCompletionRequest
		if err := json.Unmarshal(rawBody, &req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "invalid request: %s"}`, err), http.StatusBadRequest)
			return
		}

		// Reject models that arent LLMs
		if cfg.Models != nil {
			if m := cfg.Models.FindModel(req.Model); m != nil && m.Type != config.ModelTypeLLM {
				http.Error(w, fmt.Sprintf(`{"error": "model %s is of type %s, not LLM"}`, req.Model, m.Type), http.StatusBadRequest)
				return
			}
		}

		// Try to find an agent core for this model
		core := registry.Get(req.Model)
		if core == nil {
			// Not an agent model — proxy it
			baseURL, apiKey, client := agent.ProxyProvider(cfg, req.Model)
			proxy.ForwardTo(w, baseURL, apiKey, "/chat/completions", rawBody, client)
			return
		}

		threadID := req.ThreadID
		if threadID == "" {
			threadID = fmt.Sprintf("anon-%s", uuid.New().String()[:12])
		}

		if len(req.Messages) == 0 {
			http.Error(w, `{"error": "no messages provided"}`, http.StatusBadRequest)
			return
		}

		logger.Info().Str("thread_id", threadID).Str("agent", core.AgentID).Int("messages", len(req.Messages)).Msg("agent chat")
		agent.StreamAgentRun(r.Context(), w, core, threadID, req.Messages, logger)
	}
}
