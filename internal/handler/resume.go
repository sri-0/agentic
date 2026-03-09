package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/sse"
	"agentic/internal/types"

	"github.com/rs/zerolog"
	"google.golang.org/genai"
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

		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// Emit synthetic tool-call announcement so the UI shows "Running..."
		requestID := fmt.Sprintf("chatcmpl-resume-%s", req.ThreadID[:12])
		cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName, req.ThreadID)

		argsJSON, _ := json.Marshal(pending.Details)
		sse.WriteSSE(w, cb.ToolCallDelta(0, pending.ToolCallID, pending.ToolName, string(argsJSON)))
		sse.WriteSSE(w, cb.Finish("tool_calls"))

		// Store the decision so the tool handler can read it
		core.HITLStore.SetDecision(req.ThreadID, req.Action)

		// Clear pending confirmation
		core.HITLStore.ClearPending(req.ThreadID)

		// Build resume message
		var actionDesc string
		switch req.Action {
		case "approved":
			actionDesc = fmt.Sprintf("The user has APPROVED the pending %s operation. Please proceed by calling %s again with the same parameters to execute the operation.", pending.ToolName, pending.ToolName)
		case "denied":
			actionDesc = fmt.Sprintf("The user has DENIED the pending %s operation. The operation was cancelled. Please acknowledge this to the user.", pending.ToolName)
		default:
			actionDesc = fmt.Sprintf("The user has %s the pending %s operation.", req.Action, pending.ToolName)
		}

		userMsg := genai.NewContentFromText(actionDesc, genai.RoleUser)

		// Ensure session exists
		if err := core.SessionManager.GetOrCreate(r.Context(), req.ThreadID); err != nil {
			logger.Error().Err(err).Msg("failed to get session for resume")
			http.Error(w, `{"error": "session not found"}`, http.StatusInternalServerError)
			return
		}

		agent.StreamAgentRun(r.Context(), w, core, req.ThreadID, userMsg, logger)
	}
}
