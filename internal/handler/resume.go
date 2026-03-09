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

		// Find the right core for this thread. Try all registered cores.
		var core *agent.Core
		for _, id := range registry.IDs() {
			c := registry.Get(id)
			if pending := c.Interrupts.Get(req.ThreadID); pending != nil {
				core = c
				break
			}
		}

		if core == nil {
			http.Error(w, `{"error": "no pending confirmation for this thread"}`, http.StatusNotFound)
			return
		}

		pending := core.Interrupts.Get(req.ThreadID)
		if pending == nil {
			http.Error(w, `{"error": "no pending confirmation for this thread"}`, http.StatusNotFound)
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
		core.Interrupts.Clear(req.ThreadID)

		// Resume via the runner with the confirmation response
		agent.StreamResumeRun(r.Context(), w, core, req.ThreadID, pending, approved, logger)
	}
}
