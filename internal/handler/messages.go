package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentic/internal/agent"
	anth "agentic/internal/anthropic"
	"agentic/internal/config"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Messages handles Anthropic-style /v1/messages requests by converting them
// to OpenAI format, proxying upstream, and converting the response back.
func Messages(cfg *config.Config, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "failed to read request body")
			return
		}

		var req anth.Request
		if err := json.Unmarshal(rawBody, &req); err != nil {
			writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %s", err))
			return
		}

		if req.Model == "" {
			writeAnthropicError(w, http.StatusBadRequest, "model is required")
			return
		}

		// Validate model type
		if cfg.Models != nil {
			if m := cfg.Models.FindModel(req.Model); m != nil && m.Type == config.ModelTypeEmbedding {
				writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("model %s is an embedding model", req.Model))
				return
			}
		}

		// Resolve upstream provider
		baseURL, apiKey, client := agent.ProxyProvider(cfg, req.Model)
		if baseURL == "" {
			writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("unknown model: %s", req.Model))
			return
		}

		// Convert Anthropic request -> OpenAI request
		openaiBody, err := anth.ToOpenAIRequest(&req)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("conversion error: %s", err))
			return
		}

		requestID := fmt.Sprintf("msg_%s", uuid.New().String()[:24])
		logger.Info().Str("model", req.Model).Bool("stream", req.Stream).Msg("anthropic messages")

		// Make upstream request
		url := strings.TrimRight(baseURL, "/") + "/chat/completions"
		upstream, err := http.NewRequest("POST", url, bytes.NewReader(openaiBody))
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "failed to create upstream request")
			return
		}
		upstream.Header.Set("Content-Type", "application/json")
		upstream.Header.Set("Authorization", "Bearer "+apiKey)

		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(upstream)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("upstream request failed: %s", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			writeAnthropicError(w, resp.StatusCode, fmt.Sprintf("upstream error: %s", string(body)))
			return
		}

		if req.Stream {
			converter := &anth.StreamConverter{
				Model:     req.Model,
				RequestID: requestID,
			}
			converter.Convert(w, resp.Body)
		} else {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				writeAnthropicError(w, http.StatusBadGateway, "failed to read upstream response")
				return
			}

			result, err := anth.ConvertNonStreaming(req.Model, requestID, body)
			if err != nil {
				writeAnthropicError(w, http.StatusInternalServerError, fmt.Sprintf("conversion error: %s", err))
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Write(result)
		}
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"message": message,
		},
	})
}
