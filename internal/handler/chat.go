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

func Chat(registry *agent.Registry, cfg *config.Config, osClient *opensearch.Client, agentConfigs map[string]*config.AgentConfig, buildOverrideCore agent.OverrideCoreFunc, logger zerolog.Logger) http.HandlerFunc {
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

		// Prefer an explicit agent selector while preserving legacy model=agent routing.
		explicitAgentID := req.RouteAgentID()
		agentID := explicitAgentID
		if agentID == "" {
			agentID = req.Model
		}
		core := registry.Get(agentID)
		if explicitAgentID != "" && core == nil {
			logger.Warn().Str("agent_id", explicitAgentID).Msg("chat: agent not found")
			http.Error(w, fmt.Sprintf(`{"error": "agent %q not found"}`, explicitAgentID), http.StatusNotFound)
			return
		}
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

		// Per-request model override: when the request selects BOTH an agent and
		// a distinct, valid model, rebuild the agent tree (root + all sub-agents)
		// with that model and use it for THIS request only. Falls back to the
		// registry core if the model is unknown or the rebuild fails.
		if buildOverrideCore != nil && req.Model != "" && req.Model != agentID && cfg.Models != nil {
			resolvedModelID := cfg.Models.ResolveModelID(req.Model)
			if model := cfg.Models.FindModel(resolvedModelID); model != nil {
				provider := model.ProviderID
				if provider == "" {
					if p := cfg.Models.FindProviderForModel(resolvedModelID); p != nil {
						provider = p.ID
					}
				}
				if agentCfg := agentConfigs[agentID]; agentCfg != nil {
					if oc, err := buildOverrideCore(agentCfg, resolvedModelID, provider); err != nil {
						logger.Warn().Err(err).
							Str("agent_id", agentID).
							Str("override_model", resolvedModelID).
							Msg("chat: model override build failed, falling back to default agent core")
					} else {
						logger.Info().
							Str("agent_id", agentID).
							Str("override_model", resolvedModelID).
							Str("override_provider", provider).
							Msg("chat: using per-request model override for agent")
						core = oc
					}
				}
			}
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
			Str("requested_model", req.Model).
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
