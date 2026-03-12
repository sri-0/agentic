package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/chat"
	"agentic/internal/config"
	"agentic/internal/proxy"
	"agentic/internal/rag"
	"agentic/internal/types"
	"agentic/pkg/db/opensearch"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

func Chat(registry *agent.Registry, cfg *config.Config, osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
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
			// Not an agent — try to proxy to upstream provider
			baseURL, apiKey, client := agent.ProxyProvider(cfg, req.Model)
			if baseURL == "" {
				http.Error(w, fmt.Sprintf(`{"error": "model %q not found"}`, req.Model), http.StatusNotFound)
				return
			}
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

		messages := req.Messages

		// Apply prompt template after system prompt (no-op if osClient nil)
		messages = chat.ApplyPromptTemplate(r.Context(), osClient, req.PromptID, messages, logger)

		// RAG augmentation (no-op if osClient nil)
		if req.UseRAG {
			messages = rag.AugmentMessages(r.Context(), osClient, messages, logger)
		}

		logger.Info().
			Str("thread_id", threadID).
			Str("agent", core.AgentID).
			Int("messages", len(messages)).
			Bool("use_rag", req.UseRAG).
			Bool("stream", req.Stream == nil || *req.Stream).
			Str("prompt_id", req.PromptID).
			Msg("agent chat")

		if req.Stream != nil && !*req.Stream {
			agent.NonStreamAgentRun(r.Context(), w, core, threadID, messages, logger)
		} else {
			agent.StreamAgentRun(r.Context(), w, core, threadID, messages, logger)
		}
	}
}
