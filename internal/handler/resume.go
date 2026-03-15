package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/types"

	"github.com/rs/zerolog"
)

func Resume(registry *agent.Registry, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ResumeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "invalid request: %s"}`, err), http.StatusBadRequest)
			return
		}

		if req.ThreadID == "" {
			http.Error(w, `{"error": "thread_id is required"}`, http.StatusBadRequest)
			return
		}

		if req.Action == "" {
			req.Action = "denied"
		}

		// Look up the pending interrupt from the shared store (any core can read it).
		anyCore := registry.Get(registry.IDs()[0])
		pending, err := anyCore.Interrupts.Get(req.ThreadID)
		if err != nil || pending == nil {
			http.Error(w, `{"error": "no pending confirmation for this thread"}`, http.StatusNotFound)
			return
		}

		// Use the stored agent ID to find the correct core.
		core := registry.Get(pending.AgentID)
		if core == nil {
			http.Error(w, fmt.Sprintf(`{"error": "agent %s not found"}`, pending.AgentID), http.StatusNotFound)
			return
		}

		approved := req.Action == "approved"

		logger.Info().
			Str("thread_id", req.ThreadID).
			Str("action", req.Action).
			Str("tool", pending.ToolName).
			Bool("approved", approved).
			Msg("resuming agent")

		// Clear the pending interrupt
		_ = core.Interrupts.Clear(req.ThreadID)

		// Resume via the runner with the confirmation response
		agent.StreamResumeRun(r.Context(), w, core, req.ThreadID, pending, approved, logger)
	}
}
