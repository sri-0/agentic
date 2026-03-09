package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/proxy"

	"github.com/rs/zerolog"
)

func Embeddings(core *agent.Core, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error": "failed to read request body"}`, http.StatusBadRequest)
			return
		}

		// Extract model from request to find the right provider
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rawBody, &req); err != nil || req.Model == "" {
			http.Error(w, `{"error": "invalid request: model is required"}`, http.StatusBadRequest)
			return
		}

		baseURL, apiKey, client := agent.ProxyProvider(core.Config, req.Model)
		if baseURL == "" {
			http.Error(w, fmt.Sprintf(`{"error": "unknown model: %s"}`, req.Model), http.StatusBadRequest)
			return
		}

		logger.Info().Str("model", req.Model).Msg("proxy embeddings")
		proxy.ForwardTo(w, baseURL, apiKey, "/embeddings", rawBody, client)
	}
}
