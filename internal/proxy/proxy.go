package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentic/internal/types"
)

// Forward proxies a chat completion request to the downstream LLM.
func Forward(w http.ResponseWriter, _ *http.Request, baseURL, apiKey string, req types.ChatCompletionRequest) {
	// Re-encode the request body
	body, err := json.Marshal(req)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "failed to marshal request: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Build upstream URL
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"

	upstream, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "failed to create upstream request: %s"}`, err), http.StatusInternalServerError)
		return
	}

	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(upstream)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "upstream request failed: %s"}`, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream response body through
	io.Copy(w, resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
