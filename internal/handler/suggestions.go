package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	internalagents "agentic/internal/agents"

	"github.com/rs/zerolog"
	"google.golang.org/adk/session"
)

type suggestionsRequest struct {
	ThreadID string          `json:"thread_id"`
	UserID   string          `json:"user_id"`
	Messages []suggestMsg    `json:"messages"`
}

type suggestMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type suggestionsResponse struct {
	Suggestion *string `json:"suggestion"` // null if no suggestion
	ThreadID   string  `json:"thread_id"`
}

// Suggestions generates a next-action suggestion based on recent conversation.
// POST /v1/suggestions
func Suggestions(internal *internalagents.Registry, sessionSvc session.Service, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req suggestionsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		if len(req.Messages) == 0 {
			http.Error(w, `{"error":"messages required"}`, http.StatusBadRequest)
			return
		}

		userID := req.UserID
		if userID == "" {
			userID = r.Header.Get("X-User-ID")
		}
		if userID == "" {
			userID = "anonymous"
		}

		// Format last few messages as input
		var sb strings.Builder
		// Only use last 5 messages for context
		start := 0
		if len(req.Messages) > 5 {
			start = len(req.Messages) - 5
		}
		for _, msg := range req.Messages[start:] {
			sb.WriteString("[")
			sb.WriteString(msg.Role)
			sb.WriteString("]: ")
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")
		}

		output, err := internal.Run(r.Context(), "suggestion", sessionSvc, userID, sb.String())
		if err != nil {
			logger.Error().Err(err).Msg("suggestion agent failed")
			http.Error(w, `{"error":"suggestion generation failed"}`, http.StatusInternalServerError)
			return
		}

		resp := suggestionsResponse{ThreadID: req.ThreadID}

		// Filter the suggestion
		suggestion := filterSuggestion(output)
		if suggestion != "" {
			resp.Suggestion = &suggestion
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// filterSuggestion applies quality filters to the raw suggestion output.
func filterSuggestion(raw string) string {
	s := strings.TrimSpace(raw)

	// Remove wrapping quotes
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}

	// Empty or silence
	if s == "" || strings.EqualFold(s, "nothing") || strings.EqualFold(s, "silence") {
		return ""
	}

	// Word count check: 2-12 words
	words := strings.Fields(s)
	if len(words) < 1 || len(words) > 12 {
		return ""
	}

	// Too long in characters
	if utf8.RuneCountInString(s) > 80 {
		return ""
	}

	// Filter evaluative phrases
	evaluative := []string{"looks good", "great job", "nice work", "well done", "thanks", "thank you", "perfect"}
	lower := strings.ToLower(s)
	for _, phrase := range evaluative {
		if strings.Contains(lower, phrase) {
			return ""
		}
	}

	// Filter questions
	if strings.HasSuffix(s, "?") {
		return ""
	}

	// Filter AI-voice
	aiPrefixes := []string{"let me", "i'll", "i will", "here's", "here is"}
	for _, prefix := range aiPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return ""
		}
	}

	return s
}
