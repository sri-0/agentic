package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/stream"
	"agentic/internal/types"

	"github.com/rs/zerolog"
)

// Resume handles POST /v1/agent/resume: it continues a run paused on a HITL
// confirmation or an interactive question. Unlike the old synchronous path
// (StreamResumeRunFormat, which streamed straight to the ResponseWriter and was
// invisible to the event log), this routes through Coordinator.Resume so the
// continuation is event-sourced: recorded in the session log, replayable via
// ?after=, visible to /v1/sessions/{id}/stream followers, and leaving the
// session handle's awaiting-input state via terminate() (C4). Question answers
// ride the resume body → confirmation payload → the question tool → the model
// (C3). Ownership is checked so a user cannot resume another user's session (H2).
func Resume(registry *agent.Registry, coord *agent.Coordinator, logger zerolog.Logger) http.HandlerFunc {
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
			// A question reply carrying answers implies approval; otherwise default
			// to denied so a bare body is a safe no-op.
			if len(req.Answers) > 0 || req.Text != "" {
				req.Action = "approved"
			} else {
				req.Action = "denied"
			}
		}

		userID := UserID(r)

		// Look up the pending interrupt from the shared store (any core can read it).
		ids := registry.IDs()
		if len(ids) == 0 {
			http.Error(w, `{"error": "no agents registered"}`, http.StatusInternalServerError)
			return
		}
		anyCore := registry.Get(ids[0])
		pending, err := anyCore.Interrupts.Get(req.ThreadID)
		if err != nil || pending == nil {
			http.Error(w, `{"error": "no pending confirmation for this thread"}`, http.StatusNotFound)
			return
		}

		// Ownership gate (H2): if the coordinator knows this session, it must
		// belong to the requesting user. A 404 (not 403) avoids leaking existence.
		if coord != nil {
			if _, ok := coord.Status(userID, req.ThreadID); !ok {
				http.Error(w, `{"error": "session not found"}`, http.StatusNotFound)
				return
			}
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
			Str("user_id", userID).
			Str("agent_id", pending.AgentID).
			Str("action", req.Action).
			Str("tool", pending.ToolName).
			Str("tool_call_id", pending.ToolCallID).
			Int("answer_groups", len(req.Answers)).
			Bool("approved", approved).
			Msg("resume: dispatching HITL/question response via coordinator")

		// Clear the pending interrupt before resuming so a concurrent resume can't
		// double-fire the same confirmation.
		_ = core.Interrupts.Clear(req.ThreadID)

		if coord == nil {
			http.Error(w, `{"error": "coordinator unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		h, err := coord.Resume(core, req.ThreadID, userID, pending, approved, req.Answers, req.Text)
		if err != nil {
			logger.Error().Err(err).Str("thread_id", req.ThreadID).Msg("resume: coordinator resume failed")
			http.Error(w, fmt.Sprintf(`{"error": "resume failed: %s"}`, err), http.StatusConflict)
			return
		}

		// Attach the client to the session log from the resumed run's StartSeq-1 so
		// the client sees ONLY this continuation's events, and the continuation is
		// recorded in the log (event-sourced) rather than streamed connection-bound.
		format := stream.ParseFormat(r.URL.Query().Get("format"))
		agent.StreamSessionAttach(r.Context(), w, format, coord, req.ThreadID, core.ModelID, core.AgentID, h.StartSeq-1, logger)
	}
}
