package mcp

import (
	"encoding/json"
	"net/http"

	"agentic/internal/config"
	"agentic/internal/handler"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

// callbackPath is the gateway route the OAuth provider redirects back to.
const callbackPath = "/v1/mcp/oauth/callback"

// redirectURLFor derives the absolute callback URL from the incoming request,
// honouring proxy headers so the value matches what the browser used. This is
// the redirect_uri registered/sent to the OAuth provider.
func redirectURLFor(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	host := r.Host
	if xf := r.Header.Get("X-Forwarded-Host"); xf != "" {
		host = xf
	}
	return scheme + "://" + host + callbackPath
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Connect handles POST /v1/mcp/{server}/connect → returns an authorize URL for
// the current user to begin the backend-held OAuth flow (PKCE + optional RFC
// 7591 dynamic registration).
func (m *Manager) Connect(logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		server := mux.Vars(r)["server"]
		userID := handler.UserID(r)

		sc, ok := m.Config(server)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown server"})
			return
		}
		if !sc.OAuth {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server is not oauth"})
			return
		}
		authURL, err := m.oauth.Authorize(r.Context(), userID, server, sc, redirectURLFor(r))
		if err != nil {
			logger.Warn().Err(err).Str("server", server).Msg("mcp: connect failed")
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"authorize_url": authURL})
	}
}

// Callback handles GET /v1/mcp/oauth/callback → validates state, exchanges the
// code for a token, and stores it. On success it returns a small HTML page (the
// browser can close); the token now lives server-side for background runs.
func (m *Manager) Callback(logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": e, "detail": q.Get("error_description")})
			return
		}
		state, code := q.Get("state"), q.Get("code")
		if state == "" || code == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing state or code"})
			return
		}
		m.mu.RLock()
		configs := make(map[string]config.MCPServerConfig, len(m.configs))
		for k, v := range m.configs {
			configs[k] = v
		}
		m.mu.RUnlock()
		userID, server, err := m.oauth.Callback(r.Context(), state, code, configs)
		if err != nil {
			logger.Warn().Err(err).Msg("mcp: oauth callback failed")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		logger.Info().Str("server", server).Str("user", userID).Msg("mcp: oauth connected")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!doctype html><html><body><h3>Connected to " + server +
			".</h3><p>You can close this window; the gateway now holds your token for background runs.</p></body></html>"))
	}
}

// List handles GET /v1/mcp → the configured servers with per-user status.
func (m *Manager) List(logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := handler.UserID(r)
		writeJSON(w, http.StatusOK, map[string]any{"servers": m.Statuses(r.Context(), userID)})
	}
}
