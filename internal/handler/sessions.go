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

		agentID := ""
		if h, ok := coord.Status(userID, sessionID); ok {
			agentID = h.AgentID
		}
		agent.StreamSessionAttach(r.Context(), w, format, coord, sessionID, "", agentID, afterSeq, logger)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
