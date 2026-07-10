package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentic/internal/agent"
	"agentic/internal/chat"
	"agentic/internal/config"
	"agentic/internal/proxy"
	"agentic/internal/rag"
	"agentic/internal/stream"
	"agentic/internal/stream/aisdk"
	"agentic/internal/types"
	"agentic/pkg/db/opensearch"
	pkgmemory "agentic/pkg/memory"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

func Chat(registry *agent.Registry, cfg *config.Config, osClient *opensearch.Client, agentConfigs map[string]*config.AgentConfig, buildOverrideCore agent.OverrideCoreFunc, coord *agent.Coordinator, logger zerolog.Logger) http.HandlerFunc {
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

			// Always re-marshal: the client may send AI SDK UIMessages (parts),
			// but upstream needs {role,content}. req.Messages is the parsed/
			// template-applied/RAG-augmented list. Force streaming since the UI
			// transport doesn't set it.
			req.Model = resolvedModel
			req.Messages = messages
			streamTrue := true
			req.Stream = &streamTrue
			body, err := json.Marshal(req)
			if err != nil {
				body = rawBody
			}

			format := stream.ParseFormat(r.URL.Query().Get("format"))
			logger.Info().Str("model", resolvedModel).Str("upstream", baseURL).Str("format", string(format)).Bool("use_rag", req.UseRAG).Msg("chat: proxying to upstream")
			if format == stream.FormatAISDK {
				proxyAISDK(w, baseURL, apiKey, body, client, resolvedModel, logger)
			} else {
				proxy.ForwardTo(w, baseURL, apiKey, "/chat/completions", body, client)
			}
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

		// Capture the RAW last-user text BEFORE memory recall augments it, so the
		// persisted/reloaded user bubble is exactly what the user typed (the model
		// still receives the augmented content). Bug B fix.
		rawUserText := lastUserContent(messages)

		// kNN long-term memory recall injection (per-user): before the agent run,
		// pull the most relevant durable memories for this user and prepend them to
		// the last user message so the agent has cross-session recall. No-op when
		// OpenSearch is absent or nothing is recalled (degradation-safe).
		messages = injectMemoryRecall(r.Context(), cfg, osClient, UserID(r), messages, logger)

		threadID := req.ThreadID
		persistThread := threadID != "" && !req.Temporary
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
			// M6: route stream:false through the coordinator too so every turn is
			// durable/event-sourced (not just streaming turns). Falls back to the
			// direct run only when no coordinator is wired. Response shape is the
			// identical OpenAI ChatCompletion JSON either way.
			if coord != nil {
				agent.NonStreamAgentRunCoordinated(r.Context(), w, coord, core, threadID, UserID(r), messages, rawUserText, saver, logger)
			} else {
				agent.NonStreamAgentRun(r.Context(), w, core, threadID, messages, saver, logger)
			}
		} else {
			format := stream.ParseFormat(r.URL.Query().Get("format"))
			if coord != nil {
				// Background, connection-decoupled run: the run continues even if
				// this client disconnects; reconnect via /v1/sessions/{id}/stream.
				agent.StreamAgentRunBackground(r.Context(), w, format, coord, core, threadID, UserID(r), messages, rawUserText, saver, logger)
			} else {
				agent.StreamAgentRunFormat(r.Context(), w, format, core, threadID, messages, saver, logger)
			}
		}
	}
}

// proxyAISDK forwards a plain-model (no-agent) request to the upstream provider
// and translates its OpenAI SSE into the AI SDK v6 UI Message Stream, so a direct
// model chat renders the same as an agent run. Errors are surfaced as assistant
// text so the UI never renders blank.
func proxyAISDK(w http.ResponseWriter, baseURL, apiKey string, body []byte, client *http.Client, model string, logger zerolog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("x-vercel-ai-ui-message-stream", "v1")

	enc := aisdk.New(stream.NewSSESink(w), model, "")

	resp, err := proxy.OpenUpstream(baseURL, apiKey, "/chat/completions", body, client)
	if err != nil {
		enc.RunStarted()
		enc.Text("Upstream request failed: " + err.Error())
		enc.RunFinished(stream.Usage{})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		logger.Warn().Int("status", resp.StatusCode).Str("body", string(raw)).Msg("chat: upstream error (aisdk proxy)")
		enc.RunStarted()
		enc.Text(upstreamErrorText(raw))
		enc.RunFinished(stream.Usage{})
		return
	}
	stream.PumpOpenAI(resp.Body, enc, model)
}

// injectMemoryRecall queries the per-user long-term memory store (OpenSearch
// kNN, text-fallback) for memories relevant to the last user message and
// prepends them to that message as a "Relevant memories" block. It is additive
// and degradation-safe: a nil OpenSearch client, an empty query, or no results
// returns the messages unchanged.
//
// NEEDS LIVE OpenSearch (and an embedding route for the kNN path) to verify
// recall end-to-end; the wiring compiles and no-ops without them.
func injectMemoryRecall(ctx context.Context, cfg *config.Config, osClient *opensearch.Client, userID string, messages []types.ChatMessage, logger zerolog.Logger) []types.ChatMessage {
	if osClient == nil || len(messages) == 0 || userID == "" {
		return messages
	}
	lastIdx := len(messages) - 1
	if messages[lastIdx].Role != "user" || messages[lastIdx].Content == "" {
		return messages
	}
	svc := pkgmemory.NewService(osClient, cfg, logger)
	entries, err := svc.Search(ctx, cfg.AppName, userID, messages[lastIdx].Content, 5)
	if err != nil || len(entries) == 0 {
		return messages
	}
	var b strings.Builder
	b.WriteString("Relevant memories about the user (from previous sessions):\n")
	for _, e := range entries {
		b.WriteString("- ")
		b.WriteString(e.Content)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(messages[lastIdx].Content)

	out := make([]types.ChatMessage, len(messages))
	copy(out, messages)
	out[lastIdx] = types.ChatMessage{Role: messages[lastIdx].Role, Content: b.String()}
	logger.Info().Int("memories", len(entries)).Msg("chat: long-term memory recall injected")
	return out
}

// lastUserContent returns the content of the last user message, or "" if none.
// Used to capture the raw user text before any augmentation (memory recall).
func lastUserContent(messages []types.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

func upstreamErrorText(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return "Error: " + e.Error.Message
	}
	return "Error: the model request failed."
}
