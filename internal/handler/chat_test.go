package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentic/internal/agent"
	"agentic/internal/config"

	"github.com/rs/zerolog"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

type mockLLM struct {
	name      string
	responses []mockResp
	callCount int
}

type mockResp struct {
	partials  []string
	text      string
	toolCalls []struct{ id, name, argsJSON string }
}

func (m *mockLLM) Name() string { return m.name }
func (m *mockLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.callCount >= len(m.responses) {
			yield(nil, fmt.Errorf("no more responses"))
			return
		}
		resp := m.responses[m.callCount]
		m.callCount++
		if stream {
			for _, t := range resp.partials {
				if !yield(&model.LLMResponse{
					Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: t}}},
					Partial: true,
				}, nil) {
					return
				}
			}
		}
		var parts []*genai.Part
		if resp.text != "" {
			parts = append(parts, &genai.Part{Text: resp.text})
		}
		for _, tc := range resp.toolCalls {
			args := map[string]any{}
			json.Unmarshal([]byte(tc.argsJSON), &args)
			parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: tc.id, Name: tc.name, Args: args}})
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete: true,
		}, nil)
	}
}

type mockCaller struct{ results map[string]map[string]any }

func (m *mockCaller) Call(name string, _ map[string]any, _, _ string) (map[string]any, error) {
	if r, ok := m.results[name]; ok {
		return r, nil
	}
	return map[string]any{}, nil
}

func newTestCore(llm *mockLLM, caller *mockCaller) *agent.Core {
	return &agent.Core{
		Model:             llm,
		Conversations:     agent.NewConversationStore(),
		HITLStore:         agent.NewHITLStore(),
		ToolCaller:        caller,
		Config:            &config.Config{AgentModelName: "test-agent", LLMBaseURL: "http://unused"},
		SystemInstruction: "test",
		Logger:            zerolog.Nop(),
	}
}

func TestChatHandler_AgentMode_StreamsSSE(t *testing.T) {
	llm := &mockLLM{
		name:      "test",
		responses: []mockResp{{partials: []string{"Hello", " there"}, text: "Hello there"}},
	}
	core := newTestCore(llm, &mockCaller{})
	handler := Chat(core, zerolog.Nop())

	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true,"thread_id":"t1"}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	// Parse SSE events
	var textParts []string
	var hasDone bool
	scanner := bufio.NewScanner(w.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "data: [DONE]" {
			hasDone = true
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt map[string]any
		json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt)
		if choices, ok := evt["choices"].([]any); ok && len(choices) > 0 {
			choice := choices[0].(map[string]any)
			delta := choice["delta"].(map[string]any)
			if c, ok := delta["content"].(string); ok && c != "" {
				textParts = append(textParts, c)
			}
		}
	}

	if !hasDone {
		t.Error("missing [DONE] sentinel")
	}
	if len(textParts) < 2 {
		t.Errorf("expected streaming tokens, got %d: %v", len(textParts), textParts)
	}
}

func TestChatHandler_ProxyMode(t *testing.T) {
	// Set up a fake upstream that returns a simple SSE stream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"proxied\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	core := newTestCore(&mockLLM{name: "test"}, &mockCaller{})
	core.Config.LLMBaseURL = upstream.URL

	handler := Chat(core, zerolog.Nop())

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "proxied") {
		t.Error("expected proxied content in response")
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Error("expected [DONE] sentinel from proxy")
	}
}

func TestChatHandler_RejectsEmptyMessages(t *testing.T) {
	core := newTestCore(&mockLLM{name: "test"}, &mockCaller{})
	handler := Chat(core, zerolog.Nop())

	body := `{"model":"test-agent","messages":[]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for empty messages, got %d", w.Code)
	}
}
