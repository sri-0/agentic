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

		// Reject models that aren't LLMs (e.g. vision, embedding)
		if cfg.Models != nil {
			if m := cfg.Models.FindModel(req.Model); m != nil && m.Type != config.ModelTypeLLM {
				http.Error(w, fmt.Sprintf(`{"error": "model %s is of type %s, not LLM"}`, req.Model, m.Type), http.StatusBadRequest)
				return
			}
		}

		if len(req.Messages) == 0 {
			http.Error(w, `{"error": "no messages provided"}`, http.StatusBadRequest)
			return
		}

		messages := req.Messages

		// Apply prompt template (no-op if osClient nil)
		messages = chat.ApplyPromptTemplate(r.Context(), osClient, req.PromptID, messages, logger)

		// RAG augmentation — runs before agent/proxy split so it works for all models
		if req.UseRAG {
			messages = rag.AugmentMessages(r.Context(), cfg, osClient, messages, logger)
		}

		// Try to find an agent core for this model
		core := registry.Get(req.Model)
		if core == nil {
			// Not an agent — proxy to upstream provider with (possibly RAG-augmented) messages
			baseURL, apiKey, client := agent.ProxyProvider(cfg, req.Model)
			if baseURL == "" {
				logger.Warn().Str("model", req.Model).Msg("chat: model not found")
				http.Error(w, fmt.Sprintf(`{"error": "model %q not found"}`, req.Model), http.StatusNotFound)
				return
			}

			// Resolve short model ID to canonical form (e.g. "gpt-4.1-nano" → "openai/gpt-4.1-nano")
			resolvedModel := req.Model
			if cfg.Models != nil {
				resolvedModel = cfg.Models.ResolveModelID(req.Model)
			}

			// Re-marshal if model ID changed or RAG augmented the messages
			if resolvedModel != req.Model || req.UseRAG {
				req.Model = resolvedModel
				req.Messages = messages
				if augmented, err := json.Marshal(req); err == nil {
					rawBody = augmented
				}
			}

			logger.Info().Str("model", resolvedModel).Str("upstream", baseURL).Bool("use_rag", req.UseRAG).Msg("chat: proxying to upstream")
			proxy.ForwardTo(w, baseURL, apiKey, "/chat/completions", rawBody, client)
			return
		}

		threadID := req.ThreadID
		persistThread := threadID != ""
		if threadID == "" {
			threadID = fmt.Sprintf("anon-%s", uuid.New().String()[:12])
		}

		var saver *chat.MessageSaver
		if persistThread {
			saver = chat.NewMessageSaver(osClient, logger)
		}

		isStream := req.Stream == nil || *req.Stream
		logger.Info().
			Str("thread_id", threadID).
			Str("agent_id", core.AgentID).
			Int("messages", len(messages)).
			Bool("use_rag", req.UseRAG).
			Bool("stream", isStream).
			Bool("persist", persistThread).
			Str("prompt_id", req.PromptID).
			Msg("chat: routing to agent")

		if !isStream {
			agent.NonStreamAgentRun(r.Context(), w, core, threadID, messages, saver, logger)
		} else {
			agent.StreamAgentRun(r.Context(), w, core, threadID, messages, saver, logger)
		}
	}
}
