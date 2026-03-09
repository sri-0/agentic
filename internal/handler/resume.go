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

		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// Emit synthetic tool-call + result so the UI sees the flow
		requestID := fmt.Sprintf("chatcmpl-resume-%s", req.ThreadID[:12])
		cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName, req.ThreadID)

		argsJSON, _ := json.Marshal(pending.Details)
		sse.WriteSSE(w, cb.ToolCallDelta(0, pending.ToolCallID, pending.ToolName, string(argsJSON)))
		sse.WriteSSE(w, cb.Finish("tool_calls"))

		// Emit tool result
		evt := types.ToolResultEvent{}
		evt.ToolResult.ToolCallID = pending.ToolCallID
		evt.ToolResult.ToolName = pending.ToolName
		evt.ToolResult.Result = result
		sse.WriteSSE(w, evt)

		// Build FunctionResponse content to feed back to the LLM
		// This satisfies OpenAI's requirement that tool_calls must be followed by tool responses
		toolResultMsg := &genai.Content{
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						Name:     pending.ToolName,
						ID:       pending.ToolCallID,
						Response: result,
					},
				},
			},
			Role: "user",
		}

		// Ensure session exists
		if err := core.SessionManager.GetOrCreate(r.Context(), req.ThreadID); err != nil {
			logger.Error().Err(err).Msg("failed to get session for resume")
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		agent.StreamAgentRun(r.Context(), w, core, req.ThreadID, toolResultMsg, logger)
	}
}
