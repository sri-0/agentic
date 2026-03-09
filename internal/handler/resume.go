package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/types"

	"github.com/rs/zerolog"
)

func Resume(core *agent.Core, logger zerolog.Logger) http.HandlerFunc {
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

		pending := core.HITLStore.GetPending(req.ThreadID)
		if pending == nil {
			http.Error(w, `{"error": "no pending confirmation for this thread"}`, http.StatusNotFound)
			return
		}

		logger.Info().
			Str("thread_id", req.ThreadID).
			Str("action", req.Action).
			Str("tool", pending.ToolName).
			Msg("resuming agent")

		// Store the decision so the tool handler can read it
		core.HITLStore.SetDecision(req.ThreadID, req.Action)
		core.HITLStore.ClearPending(req.ThreadID)

		// Execute the tool with the stored decision to get the actual result
		args, _ := pending.Details.(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, err := core.ToolCaller.Call(pending.ToolName, args, req.ThreadID, pending.ToolCallID)
		if err != nil {
			result = map[string]any{"error": err.Error()}
		}

		agent.StreamResumeRun(r.Context(), w, core, req.ThreadID, pending.ToolCallID, pending.ToolName, args, result, logger)
	}
}
