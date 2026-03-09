package handler

import (
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

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// newFakeUpstream creates a fake OpenAI-compatible upstream server.
func newFakeUpstream(t *testing.T, streaming bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		if req["stream"] == true && streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there!\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":    "chatcmpl-test",
				"model": req["model"],
				"choices": []map[string]any{{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "Hello there!",
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     10,
					"completion_tokens": 5,
					"total_tokens":      15,
				},
			})
		}
	}))
}

// newFakeToolCallUpstream creates a fake upstream that returns tool calls.
func newFakeToolCallUpstream(t *testing.T, streaming bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		if req["stream"] == true && streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-tc\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Let me check.\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-tc\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_123\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-tc\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\"\"}}]},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-tc\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\": \\\"SF\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-tc\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":    "chatcmpl-tc",
				"model": req["model"],
				"choices": []map[string]any{{
					"index":         0,
					"finish_reason": "tool_calls",
					"message": map[string]any{
						"role":    "assistant",
						"content": "Let me check.",
						"tool_calls": []map[string]any{{
							"id":   "call_123",
							"type": "function",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": `{"city": "SF"}`,
							},
						}},
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     15,
					"completion_tokens": 10,
					"total_tokens":      25,
				},
			})
		}
	}))
}

func newMessagesTestCore(t *testing.T, upstream *httptest.Server) *agent.Core {
	t.Helper()

	t.Setenv("TEST_API_KEY", "test-key")

	a, err := adkagent.New(adkagent.Config{
		Name: "test_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "test_app",
		Agent:          a,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatal(err)
	}

	sm := agent.NewSessionManager(sessionService, "test_app", zerolog.Nop())

	return &agent.Core{
		Runner:         r,
		SessionManager: sm,
		Interrupts:     agent.NewInterruptStore(),
		AgentID:        "test-agent",
		Config: &config.Config{
			AppName: "test_app",
			Agents: &config.AgentsConfig{
				Agents: []config.AgentConfig{{ID: "test-agent"}},
			},
			Models: &config.ModelsConfig{
				Providers: []config.Provider{{
					ID:        "test",
					Name:      "Test",
					BaseURL:   upstream.URL,
					APIKeyEnv: "TEST_API_KEY",
					Models: []config.Model{
						{ID: "claude-sonnet-4-20250514", Type: config.ModelTypeLLM, OwnedBy: "anthropic"},
					},
				}},
			},
		},
		Logger: zerolog.Nop(),
	}
}

// newAnthropicClient creates an Anthropic SDK client pointed at our test server.
func newAnthropicClient(serverURL string) anthropic.Client {
	return anthropic.NewClient(
		option.WithBaseURL(serverURL+"/v1"),
		option.WithAPIKey("test-key"),
	)
}

func TestMessages_NonStreaming(t *testing.T) {
	upstream := newFakeUpstream(t, false)
	defer upstream.Close()

	core := newMessagesTestCore(t, upstream)
	handler := Messages(core.Config, zerolog.Nop())
	server := httptest.NewServer(handler)
	defer server.Close()

	client := newAnthropicClient(server.URL)

	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Hello")),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Type != "message" {
		t.Errorf("expected type 'message', got %q", msg.Type)
	}
	if msg.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", msg.Role)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("expected stop_reason 'end_turn', got %q", msg.StopReason)
	}
	if len(msg.Content) == 0 {
		t.Fatal("expected content blocks")
	}

	block := msg.Content[0]
	if text := block.AsText(); text.Text != "Hello there!" {
		t.Errorf("expected 'Hello there!', got %q", text.Text)
	}
}

func TestMessages_NonStreaming_ToolUse(t *testing.T) {
	upstream := newFakeToolCallUpstream(t, false)
	defer upstream.Close()

	core := newMessagesTestCore(t, upstream)
	handler := Messages(core.Config, zerolog.Nop())
	server := httptest.NewServer(handler)
	defer server.Close()

	client := newAnthropicClient(server.URL)

	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("What's the weather?")),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", msg.StopReason)
	}

	if len(msg.Content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %d", len(msg.Content))
	}

	// First block: text
	textBlock := msg.Content[0]
	if textBlock.AsText().Text != "Let me check." {
		t.Errorf("expected text 'Let me check.', got %q", textBlock.AsText().Text)
	}

	// Second block: tool_use
	toolBlock := msg.Content[1]
	tu := toolBlock.AsToolUse()
	if tu.Name != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %q", tu.Name)
	}
}

func TestMessages_Streaming(t *testing.T) {
	upstream := newFakeUpstream(t, true)
	defer upstream.Close()

	core := newMessagesTestCore(t, upstream)
	handler := Messages(core.Config, zerolog.Nop())
	server := httptest.NewServer(handler)
	defer server.Close()

	client := newAnthropicClient(server.URL)

	stream := client.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Hello")),
		},
	})

	var (
		gotMessageStart bool
		gotContentStart bool
		gotContentDelta bool
		gotContentStop  bool
		gotMessageDelta bool
		gotMessageStop  bool
		textParts       []string
	)

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "message_start":
			gotMessageStart = true
			if event.Message.Role != "assistant" {
				t.Errorf("expected role assistant in message_start")
			}
		case "content_block_start":
			gotContentStart = true
		case "content_block_delta":
			gotContentDelta = true
			if event.Delta.Type == "text_delta" {
				textParts = append(textParts, event.Delta.Text)
			}
		case "content_block_stop":
			gotContentStop = true
		case "message_delta":
			gotMessageDelta = true
			if event.Delta.StopReason != "end_turn" {
				t.Errorf("expected stop_reason 'end_turn', got %q", event.Delta.StopReason)
			}
		case "message_stop":
			gotMessageStop = true
		}
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if !gotMessageStart {
		t.Error("missing message_start event")
	}
	if !gotContentStart {
		t.Error("missing content_block_start event")
	}
	if !gotContentDelta {
		t.Error("missing content_block_delta event")
	}
	if !gotContentStop {
		t.Error("missing content_block_stop event")
	}
	if !gotMessageDelta {
		t.Error("missing message_delta event")
	}
	if !gotMessageStop {
		t.Error("missing message_stop event")
	}

	combined := strings.Join(textParts, "")
	if combined != "Hello there!" {
		t.Errorf("expected streamed text 'Hello there!', got %q", combined)
	}
}

func TestMessages_Streaming_ToolUse(t *testing.T) {
	upstream := newFakeToolCallUpstream(t, true)
	defer upstream.Close()

	core := newMessagesTestCore(t, upstream)
	handler := Messages(core.Config, zerolog.Nop())
	server := httptest.NewServer(handler)
	defer server.Close()

	client := newAnthropicClient(server.URL)

	stream := client.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Weather?")),
		},
	})

	var (
		gotToolStart    bool
		gotToolDelta    bool
		toolName        string
		stopReason      string
		jsonParts       []string
	)

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				gotToolStart = true
				toolName = event.ContentBlock.Name
			}
		case "content_block_delta":
			if event.Delta.Type == "input_json_delta" {
				gotToolDelta = true
				jsonParts = append(jsonParts, event.Delta.PartialJSON)
			}
		case "message_delta":
			stopReason = string(event.Delta.StopReason)
		}
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if !gotToolStart {
		t.Error("missing tool_use content_block_start")
	}
	if toolName != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %q", toolName)
	}
	if !gotToolDelta {
		t.Error("missing input_json_delta events")
	}
	if stopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", stopReason)
	}

	combined := strings.Join(jsonParts, "")
	if !strings.Contains(combined, "SF") {
		t.Errorf("expected tool args to contain 'SF', got %q", combined)
	}
}

func TestMessages_RejectsEmbeddingModel(t *testing.T) {
	upstream := newFakeUpstream(t, false)
	defer upstream.Close()

	core := newMessagesTestCore(t, upstream)
	// Add an embedding model
	core.Config.Models.Providers[0].Models = append(core.Config.Models.Providers[0].Models,
		config.Model{ID: "text-embedding-3-small", Type: config.ModelTypeEmbedding, OwnedBy: "openai"},
	)

	handler := Messages(core.Config, zerolog.Nop())
	server := httptest.NewServer(handler)
	defer server.Close()

	client := newAnthropicClient(server.URL)

	_, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "text-embedding-3-small",
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Hello")),
		},
	})

	if err == nil {
		t.Fatal("expected error for embedding model")
	}

	// Suppress unused import warnings
	_ = model.LLMResponse{}
	_ = genai.Content{}
}
