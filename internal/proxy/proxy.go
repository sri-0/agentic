package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Forward proxies a raw chat completion request body to the downstream LLM.
func Forward(w http.ResponseWriter, baseURL, apiKey string, rawBody []byte) {
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"

	upstream, err := http.NewRequest("POST", url, bytes.NewReader(rawBody))
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

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	io.Copy(w, resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
