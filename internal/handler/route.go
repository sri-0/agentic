package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"agentic/internal/agents"
	"agentic/internal/config"
	"agentic/internal/types"

	"google.golang.org/adk/session"

	"github.com/rs/zerolog"
)

// RouteRequest asks the router which agent should handle a message.
type RouteRequest struct {
	Messages []types.ChatMessage `json:"messages"`
	Message  string              `json:"message"`
}

// Route classifies a request to the best primary agent via the internal router
// agent. Returns {"agent_id": "..."}. This is the auto-router/classifier
// (separate from suggestions) — usable directly, and the seam for agent_id="auto".
func Route(internalAgents *agents.Registry, cfg *config.Config, sessionSvc session.Service, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := UserID(r)
		var req RouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		msg := req.Message
		if msg == "" && len(req.Messages) > 0 {
			msg = req.Messages[len(req.Messages)-1].Content
		}
		if msg == "" {
			http.Error(w, `{"error":"no message"}`, http.StatusBadRequest)
			return
		}

		// Build the candidate list from the primary (non-internal) roster.
		var b strings.Builder
		b.WriteString("Available agents:\n")
		fallback := ""
		if cfg.Agents != nil {
			for _, a := range cfg.Agents.Agents {
				if a.Internal {
					continue
				}
				if fallback == "" {
					fallback = a.ID
				}
				b.WriteString("- ")
				b.WriteString(a.ID)
				b.WriteString(": ")
				b.WriteString(a.Description)
				b.WriteByte('\n')
			}
		}
		b.WriteString("\nUser message: ")
		b.WriteString(msg)
		b.WriteString("\n\nBest agent id:")

		chosen := fallback
		if internalAgents != nil {
			out, err := internalAgents.Run(r.Context(), "router", sessionSvc, userID, b.String())
			if err != nil {
				logger.Warn().Err(err).Msg("route: router agent failed, using fallback")
			} else if id := normaliseAgentID(out, cfg); id != "" {
				chosen = id
			}
		}
		writeJSON(w, map[string]any{"agent_id": chosen})
	}
}

// normaliseAgentID extracts a valid agent id from the router's raw output.
func normaliseAgentID(raw string, cfg *config.Config) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\"'`.")
	if cfg.Agents == nil {
		return raw
	}
	// Exact match first.
	for _, a := range cfg.Agents.Agents {
		if a.ID == raw {
			return a.ID
		}
	}
	// Otherwise, the first id that appears in the output.
	for _, a := range cfg.Agents.Agents {
		if !a.Internal && strings.Contains(raw, a.ID) {
			return a.ID
		}
	}
	return ""
}
