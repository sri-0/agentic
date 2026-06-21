package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenUpstream performs the upstream POST and returns the live response so the
// caller can transform the stream (e.g. OpenAI SSE → AI SDK v6). The caller owns
// closing resp.Body. Returns the response even on non-2xx so the caller can
// surface the error body.
func OpenUpstream(baseURL, apiKey, path string, rawBody []byte, client *http.Client) (*http.Response, error) {
	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequest("POST", url, bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

// ForwardTo proxies a raw request body to baseURL+path.
// For SSE streams, it flushes each chunk as it arrives from upstream.
// If client is nil, http.DefaultClient is used.
func ForwardTo(w http.ResponseWriter, baseURL, apiKey, path string, rawBody []byte, client *http.Client) {
	url := strings.TrimRight(baseURL, "/") + path

	upstream, err := http.NewRequest("POST", url, bytes.NewReader(rawBody))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "failed to create upstream request: %s"}`, err), http.StatusInternalServerError)
		return
	}

	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+apiKey)

	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(upstream)
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

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				// Log but don't return error to client mid-stream
			}
			break
		}
	}
}
