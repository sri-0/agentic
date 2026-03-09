package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentic/internal/config"
	"agentic/internal/types"

	"github.com/rs/zerolog"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// mockLLM implements model.LLM for testing.
type mockLLM struct {
	name      string
	responses []mockResponse
	callCount int
}

type mockResponse struct {
	// Partial text chunks to stream before the final response
	partialTexts []string
	// Final response content
	text      string
	toolCalls []mockToolCall
}

type mockToolCall struct {
	id   string
	name string
	args map[string]any
}

func (m *mockLLM) Name() string { return m.name }

func (m *mockLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.callCount >= len(m.responses) {
			yield(nil, fmt.Errorf("unexpected call %d", m.callCount))
			return
		}
		resp := m.responses[m.callCount]
		m.callCount++

		if stream {
			// Yield partial text tokens for streaming
			for _, text := range resp.partialTexts {
				partial := &model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: text}},
					},
					Partial:      true,
					TurnComplete: false,
				}
				if !yield(partial, nil) {
					return
				}
			}
		}

		// Yield final response
		var parts []*genai.Part
		if resp.text != "" {
			parts = append(parts, &genai.Part{Text: resp.text})
		}
		for _, tc := range resp.toolCalls {
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.id,
					Name: tc.name,
					Args: tc.args,
				},
			})
		}

		final := &model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: parts,
			},
			Partial:      false,
			TurnComplete: true,
		}
		yield(final, nil)
	}
}

// mockToolCaller implements ToolCaller for testing.
type mockToolCaller struct {
	results map[string]map[string]any
}

func (m *mockToolCaller) Call(name string, _ map[string]any, _, _ string) (map[string]any, error) {
	if result, ok := m.results[name]; ok {
		return result, nil
	}
	return map[string]any{"error": "unknown tool"}, nil
}

func newTestCore(llm *mockLLM, toolCaller *mockToolCaller) *Core {
	return &Core{
		Model:         llm,
		ToolDecls:     nil,
		Conversations: NewConversationStore(),
		HITLStore:     NewHITLStore(),
		ToolCaller:    toolCaller,
		Config: &config.Config{
			AgentModelName: "test-agent",
		},
		SystemInstruction: "You are a test assistant.",
		Logger:            zerolog.Nop(),
	}
}

// parseSSEEvents parses the SSE response body into individual JSON objects.
func parseSSEEvents(body string) []map[string]any {
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(data), &parsed); err == nil {
			events = append(events, parsed)
		}
	}
	return events
}

func TestStreamAgentRun_SimpleTextResponse(t *testing.T) {
	llm := &mockLLM{
		name: "test-model",
		responses: []mockResponse{
			{
				partialTexts: []string{"Hello", " world", "!"},
				text:         "Hello world!",
			},
		},
	}
	core := newTestCore(llm, &mockToolCaller{})
	w := httptest.NewRecorder()
	logger := zerolog.Nop()

	messages := []types.ChatMessage{
		{Role: "user", Content: "Hi"},
	}

	StreamAgentRun(context.Background(), w, core, "thread-1", messages, logger)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify SSE headers
	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	events := parseSSEEvents(w.Body.String())
	if len(events) == 0 {
		t.Fatal("expected SSE events, got none")
	}

	// Check we got streaming text deltas
	var textDeltas []string
	var hasFinishStop bool
	var hasProgress bool

	for _, evt := range events {
		if _, ok := evt["agent_progress"]; ok {
			hasProgress = true
			continue
		}
		choices, ok := evt["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			textDeltas = append(textDeltas, content)
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr == "stop" {
			hasFinishStop = true
		}
	}

	if !hasProgress {
		t.Error("expected agent_progress event")
	}
	if len(textDeltas) < 3 {
		t.Errorf("expected at least 3 streaming text deltas, got %d: %v", len(textDeltas), textDeltas)
	}
	if !hasFinishStop {
		t.Error("expected finish_reason=stop")
	}

	// Verify body ends with [DONE]
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Error("expected data: [DONE] sentinel")
	}
}

func TestStreamAgentRun_ToolCallAndResult(t *testing.T) {
	llm := &mockLLM{
		name: "test-model",
		responses: []mockResponse{
			{
				// First call: LLM wants to use a tool
				toolCalls: []mockToolCall{
					{id: "call_001", name: "query_database", args: map[string]any{"sql": "SELECT * FROM products"}},
				},
			},
			{
				// Second call: LLM responds with final text after tool result
				partialTexts: []string{"Here are ", "the results."},
				text:         "Here are the results.",
			},
		},
	}
	toolCaller := &mockToolCaller{
		results: map[string]map[string]any{
			"query_database": {"table": "products", "row_count": 5, "rows": []any{}},
		},
	}
	core := newTestCore(llm, toolCaller)
	w := httptest.NewRecorder()
	logger := zerolog.Nop()

	messages := []types.ChatMessage{
		{Role: "user", Content: "Show me all products"},
	}

	StreamAgentRun(context.Background(), w, core, "thread-2", messages, logger)

	events := parseSSEEvents(w.Body.String())

	var hasToolCall bool
	var hasToolResult bool
	var hasToolCallsFinish bool
	var hasStopFinish bool
	var textDeltas []string

	for _, evt := range events {
		if _, ok := evt["tool_result"]; ok {
			hasToolResult = true
			tr := evt["tool_result"].(map[string]any)
			if tr["toolCallId"] != "call_001" {
				t.Errorf("expected toolCallId call_001, got %v", tr["toolCallId"])
			}
			continue
		}
		choices, ok := evt["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)

		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasToolCall = true
			tc := tcs[0].(map[string]any)
			if tc["id"] != "call_001" {
				t.Errorf("expected tool call id call_001, got %v", tc["id"])
			}
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			textDeltas = append(textDeltas, content)
		}
		if fr, ok := choice["finish_reason"].(string); ok {
			if fr == "tool_calls" {
				hasToolCallsFinish = true
			}
			if fr == "stop" {
				hasStopFinish = true
			}
		}
	}

	if !hasToolCall {
		t.Error("expected tool_call delta")
	}
	if !hasToolCallsFinish {
		t.Error("expected finish_reason=tool_calls")
	}
	if !hasToolResult {
		t.Error("expected tool_result event")
	}
	if len(textDeltas) < 2 {
		t.Errorf("expected streaming text deltas after tool, got %d", len(textDeltas))
	}
	if !hasStopFinish {
		t.Error("expected finish_reason=stop")
	}

	// Verify 2 LLM calls were made
	if llm.callCount != 2 {
		t.Errorf("expected 2 LLM calls, got %d", llm.callCount)
	}
}

func TestStreamAgentRun_HITLInterrupt(t *testing.T) {
	llm := &mockLLM{
		name: "test-model",
		responses: []mockResponse{
			{
				toolCalls: []mockToolCall{
					{id: "call_hitl", name: "write_database", args: map[string]any{"table": "users", "operation": "insert"}},
				},
			},
		},
	}
	toolCaller := &mockToolCaller{
		results: map[string]map[string]any{
			"write_database": {
				"requires_confirmation": true,
				"prompt":                "Allow insert on users?",
				"details":              map[string]any{"table": "users"},
			},
		},
	}
	core := newTestCore(llm, toolCaller)
	w := httptest.NewRecorder()
	logger := zerolog.Nop()

	messages := []types.ChatMessage{
		{Role: "user", Content: "Insert a new user"},
	}

	StreamAgentRun(context.Background(), w, core, "thread-hitl", messages, logger)

	events := parseSSEEvents(w.Body.String())

	var hasInterrupt bool
	for _, evt := range events {
		if ti, ok := evt["tool_interrupt"]; ok {
			hasInterrupt = true
			interrupt := ti.(map[string]any)
			if interrupt["toolCallId"] != "call_hitl" {
				t.Errorf("expected toolCallId call_hitl, got %v", interrupt["toolCallId"])
			}
			if interrupt["toolName"] != "write_database" {
				t.Errorf("expected toolName write_database, got %v", interrupt["toolName"])
			}
			if interrupt["thread_id"] != "thread-hitl" {
				t.Errorf("expected thread_id thread-hitl, got %v", interrupt["thread_id"])
			}
		}
	}

	if !hasInterrupt {
		t.Error("expected tool_interrupt event")
	}

	// Verify conversation was saved for resume
	saved := core.Conversations.Get("thread-hitl")
	if len(saved) == 0 {
		t.Error("expected conversation to be saved for resume")
	}

	// Verify HITL store has pending confirmation
	pending := core.HITLStore.GetPending("thread-hitl")
	if pending == nil {
		t.Fatal("expected pending HITL confirmation")
	}
	if pending.ToolCallID != "call_hitl" {
		t.Errorf("expected pending tool call id call_hitl, got %s", pending.ToolCallID)
	}
}

func TestStreamAgentRun_StreamingTimestamps(t *testing.T) {
	// Verify that partial text tokens are emitted incrementally, not all at once
	llm := &mockLLM{
		name: "test-model",
		responses: []mockResponse{
			{
				partialTexts: []string{"tok1", "tok2", "tok3", "tok4", "tok5"},
				text:         "tok1tok2tok3tok4tok5",
			},
		},
	}
	core := newTestCore(llm, &mockToolCaller{})
	w := httptest.NewRecorder()
	logger := zerolog.Nop()

	start := time.Now()
	StreamAgentRun(context.Background(), w, core, "thread-ts", []types.ChatMessage{{Role: "user", Content: "test"}}, logger)
	elapsed := time.Since(start)

	events := parseSSEEvents(w.Body.String())
	var textCount int
	for _, evt := range events {
		choices, ok := evt["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			textCount++
		}
	}

	if textCount < 5 {
		t.Errorf("expected at least 5 text delta events (streaming), got %d", textCount)
	}

	// Should complete quickly since it's a mock (no network delay)
	if elapsed > 5*time.Second {
		t.Errorf("streaming took too long (%v), possible buffering issue", elapsed)
	}
}

func TestStreamResumeRun(t *testing.T) {
	llm := &mockLLM{
		name: "test-model",
		responses: []mockResponse{
			{
				partialTexts: []string{"Done! ", "Record inserted."},
				text:         "Done! Record inserted.",
			},
		},
	}
	core := newTestCore(llm, &mockToolCaller{})

	// Pre-populate conversation store (simulating saved state from HITL interrupt)
	core.Conversations.Append("thread-resume",
		&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "Insert a user"}}},
		&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: "call_r1", Name: "write_database", Args: map[string]any{"table": "users"}},
		}}},
	)

	w := httptest.NewRecorder()
	logger := zerolog.Nop()

	toolResult := map[string]any{"success": true, "rows_affected": 1}
	StreamResumeRun(context.Background(), w, core, "thread-resume",
		"call_r1", "write_database", map[string]any{"table": "users"}, toolResult, logger)

	events := parseSSEEvents(w.Body.String())

	var hasToolCall bool
	var hasToolResult bool
	var textDeltas []string

	for _, evt := range events {
		if _, ok := evt["tool_result"]; ok {
			hasToolResult = true
		}
		choices, ok := evt["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasToolCall = true
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			textDeltas = append(textDeltas, content)
		}
	}

	if !hasToolCall {
		t.Error("expected synthetic tool_call delta for resume")
	}
	if !hasToolResult {
		t.Error("expected tool_result event for resume")
	}
	if len(textDeltas) < 2 {
		t.Errorf("expected streaming text after resume, got %d", len(textDeltas))
	}
}

func TestMessageToContents(t *testing.T) {
	messages := []types.ChatMessage{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there"},
		{Role: "system", Content: "ignored"},
		{Role: "user", Content: "How are you?"},
	}

	contents := messagesToContents(messages)
	if len(contents) != 3 {
		t.Fatalf("expected 3 contents (system filtered), got %d", len(contents))
	}
	if contents[0].Role != genai.RoleUser {
		t.Errorf("expected user role, got %s", contents[0].Role)
	}
	if contents[1].Role != genai.RoleModel {
		t.Errorf("expected model role, got %s", contents[1].Role)
	}
	if contents[2].Role != genai.RoleUser {
		t.Errorf("expected user role, got %s", contents[2].Role)
	}
}

func TestConversationStore(t *testing.T) {
	store := NewConversationStore()

	if got := store.Get("thread-1"); len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}

	store.Append("thread-1", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "hi"}}})
	if got := store.Get("thread-1"); len(got) != 1 {
		t.Errorf("expected 1, got %d", len(got))
	}

	store.Clear("thread-1")
	if got := store.Get("thread-1"); len(got) != 0 {
		t.Errorf("expected empty after clear, got %d", len(got))
	}
}
