package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"agentic/internal/agent"
	"agentic/internal/stream"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

// SessionsList returns the user's known runs (running + recently finished), so
// the UI can show still-running sessions and let the user rejoin them.
func SessionsList(coord *agent.Coordinator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := UserID(r)
		handles := coord.List(userID)
		writeJSON(w, map[string]any{"object": "list", "data": handles})
	}
}

// SessionStatus returns the status of one session.
func SessionStatus(coord *agent.Coordinator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := UserID(r)
		sessionID := mux.Vars(r)["id"]
		h, ok := coord.Status(userID, sessionID)
		if !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, h)
	}
}

// SessionStream attaches to a session's event log and streams it live. With
// ?after=<seq> it resumes exactly-once from that sequence (replay-then-live).
// This is the reconnect endpoint: a client that dropped mid-run rejoins here.
func SessionStream(coord *agent.Coordinator, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := UserID(r)
		sessionID := mux.Vars(r)["id"]

		var afterSeq int64
		if v := r.URL.Query().Get("after"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				// H1: a negative/invalid after must not reach the log (it would
				// panic on a negative slice index and poison the session mutex).
				http.Error(w, `{"error":"invalid 'after' parameter: must be a non-negative integer"}`, http.StatusBadRequest)
				return
			}
			afterSeq = n
		}
		format := stream.ParseFormat(r.URL.Query().Get("format"))

		// H2: only attach if this session is known AND owned by the requesting
		// user. A miss (unknown id or wrong owner) is a 404 — never stream another
		// user's log. 404 (not 403) avoids leaking session existence.
		h, ok := coord.Status(userID, sessionID)
		if !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		agent.StreamSessionAttach(r.Context(), w, format, coord, sessionID, "", h.AgentID, afterSeq, logger)
	}
}

// SessionCancel stops an active run for a session the requesting user owns
// (POST /v1/sessions/{id}/cancel). Ownership is checked before cancelling so a
// user cannot cancel another user's run (H2). Cancel routes through the
// coordinator's once-guarded terminate path so the terminal is idempotent.
func SessionCancel(coord *agent.Coordinator, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := UserID(r)
		sessionID := mux.Vars(r)["id"]

		// Ownership gate: 404 unless this session is known and owned by the user.
		if _, ok := coord.Status(userID, sessionID); !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}

		cancelled := coord.Cancel(sessionID)
		logger.Info().Str("session", sessionID).Str("user_id", userID).Bool("cancelled", cancelled).Msg("session cancel requested")
		writeJSON(w, map[string]any{"session_id": sessionID, "cancelled": cancelled, "status": "cancelled"})
	}
}

// SessionMarkViewed marks a session as viewed by its owner (Task B,
// POST /v1/sessions/{id}/viewed). Ownership is enforced via UserID(r): a session
// unknown to the user or owned by someone else is a 404 (mirrors cancel). On
// success the session's viewed flag flips to true, so GET /v1/sessions no longer
// renders the completed-but-unseen ring.
func SessionMarkViewed(coord *agent.Coordinator, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := UserID(r)
		sessionID := mux.Vars(r)["id"]

		if !coord.MarkViewed(userID, sessionID) {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		logger.Info().Str("session", sessionID).Str("user_id", userID).Msg("session marked viewed")
		writeJSON(w, map[string]any{"session_id": sessionID, "viewed": true})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
