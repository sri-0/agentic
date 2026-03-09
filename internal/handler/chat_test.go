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
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

func newTestCore(t *testing.T, customAgent adkagent.Agent) *agent.Core {
	t.Helper()

	sessionService := session.InMemoryService()
	appName := "test_app"

	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          customAgent,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatal(err)
	}

	sm := agent.NewSessionManager(sessionService, appName, zerolog.Nop())

	return &agent.Core{
		Runner:         r,
		SessionManager: sm,
		Interrupts:     agent.NewInterruptStore(),
		Config: &config.Config{
			AgentModelName: "test-agent",
			AppName:        appName,
			LLMBaseURL:     "http://unused",
		},
		Logger: zerolog.Nop(),
	}
}

func makeTextAgent(t *testing.T) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "test_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				for _, text := range []string{"Hello", " there"} {
					evt := session.NewEvent(ctx.InvocationID())
					evt.LLMResponse = model.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{Text: text}},
						},
						Partial: true,
					}
					evt.Author = "test_agent"
					if !yield(evt, nil) {
						return
					}
				}
				finalEvt := session.NewEvent(ctx.InvocationID())
				finalEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Hello there"}},
					},
					TurnComplete: true,
				}
				finalEvt.Author = "test_agent"
				yield(finalEvt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestChatHandler_AgentMode_StreamsSSE(t *testing.T) {
	core := newTestCore(t, makeTextAgent(t))
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

	core := newTestCore(t, makeTextAgent(t))
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
	core := newTestCore(t, makeTextAgent(t))
	handler := Chat(core, zerolog.Nop())

	body := `{"model":"test-agent","messages":[]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for empty messages, got %d", w.Code)
	}
}

func TestResumeHandler(t *testing.T) {
	resumeAgent, err := adkagent.New(adkagent.Config{
		Name: "resume_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				evt := session.NewEvent(ctx.InvocationID())
				evt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Approved and done."}},
					},
					TurnComplete: true,
				}
				evt.Author = "resume_agent"
				yield(evt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	core := newTestCore(t, resumeAgent)

	// Pre-create session
	if err := core.SessionManager.GetOrCreate(context.Background(), "thread-resume"); err != nil {
		t.Fatal(err)
	}

	// Set pending interrupt
	core.Interrupts.Set("thread-resume", &agent.PendingInterrupt{
		ConfirmationCallID: "confirm_001",
		ToolCallID:         "call_001",
		ToolName:           "write_database",
		Prompt:             "Approve write?",
		Details:            map[string]any{"table": "users"},
	})

	handler := Resume(core, zerolog.Nop())

	body := `{"thread_id":"thread-resume","action":"approved"}`
	req := httptest.NewRequest("POST", "/v1/agent/resume", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify SSE stream
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Error("expected [DONE] sentinel")
	}

	// Verify interrupt was cleared
	if pending := core.Interrupts.Get("thread-resume"); pending != nil {
		t.Error("expected pending interrupt to be cleared after resume")
	}
}

func TestResumeHandler_NoPending(t *testing.T) {
	core := newTestCore(t, makeTextAgent(t))
	handler := Resume(core, zerolog.Nop())

	body := `{"thread_id":"nonexistent","action":"approved"}`
	req := httptest.NewRequest("POST", "/v1/agent/resume", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}
