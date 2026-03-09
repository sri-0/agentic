package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"iter"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentic/internal/config"

	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"agentic/internal/types"
)

// newTestCore creates a Core with a custom agent for testing.
func newTestCore(t *testing.T, customAgent adkagent.Agent) *Core {
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

	sm := NewSessionManager(sessionService, appName, zerolog.Nop())

	return &Core{
		Runner:         r,
		SessionManager: sm,
		Interrupts:     NewInterruptStore(),
		AgentID:        "test-agent",
		Config: &config.Config{
			AppName: appName,
		},
		Logger: zerolog.Nop(),
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

// makeTextAgent creates an agent that yields streaming partial events then a final text.
func makeTextAgent(t *testing.T, partials []string, finalText string) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "text_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				// Yield partial streaming events
				for _, text := range partials {
					evt := session.NewEvent(ctx.InvocationID())
					evt.LLMResponse = model.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{Text: text}},
						},
						Partial: true,
					}
					evt.Author = "text_agent"
					if !yield(evt, nil) {
						return
					}
				}
				// Yield final response
				evt := session.NewEvent(ctx.InvocationID())
				evt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: finalText}},
					},
					TurnComplete: true,
				}
				evt.Author = "text_agent"
				yield(evt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// makeToolCallAgent creates an agent that yields a tool call, then a tool response,
// then final text.
func makeToolCallAgent(t *testing.T) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "tool_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				// Yield tool call event
				callEvt := session.NewEvent(ctx.InvocationID())
				callEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_001",
								Name: "query_database",
								Args: map[string]any{"sql": "SELECT * FROM products"},
							},
						}},
					},
				}
				callEvt.Author = "tool_agent"
				if !yield(callEvt, nil) {
					return
				}

				// Yield tool response event
				respEvt := session.NewEvent(ctx.InvocationID())
				respEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionResponse: &genai.FunctionResponse{
								ID:       "call_001",
								Name:     "query_database",
								Response: map[string]any{"row_count": 5, "table": "products"},
							},
						}},
					},
				}
				respEvt.Author = "tool_agent"
				if !yield(respEvt, nil) {
					return
				}

				// Yield streaming text partials
				for _, text := range []string{"Here are ", "the results."} {
					partial := session.NewEvent(ctx.InvocationID())
					partial.LLMResponse = model.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{Text: text}},
						},
						Partial: true,
					}
					partial.Author = "tool_agent"
					if !yield(partial, nil) {
						return
					}
				}

				// Final text
				finalEvt := session.NewEvent(ctx.InvocationID())
				finalEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Here are the results."}},
					},
					TurnComplete: true,
				}
				finalEvt.Author = "tool_agent"
				yield(finalEvt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// makeHITLAgent creates an agent that yields a tool call followed by an
// adk_request_confirmation event, simulating the ADK HITL flow.
func makeHITLAgent(t *testing.T) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "hitl_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				// Yield the confirmation request event (adk_request_confirmation)
				confirmEvt := session.NewEvent(ctx.InvocationID())
				confirmEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionCall: &genai.FunctionCall{
								ID:   "confirm_001",
								Name: toolconfirmation.FunctionCallName,
								Args: map[string]any{
									"hint": "Please approve write_database operation",
									"originalFunctionCall": &genai.FunctionCall{
										ID:   "call_write_001",
										Name: "write_database",
										Args: map[string]any{"table": "users", "operation": "insert"},
									},
								},
							},
						}},
					},
				}
				confirmEvt.Actions.SkipSummarization = true
				confirmEvt.Author = "hitl_agent"
				yield(confirmEvt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStreamAgentRun_SimpleTextResponse(t *testing.T) {
	agent := makeTextAgent(t, []string{"Hello", " world", "!"}, "Hello world!")
	core := newTestCore(t, agent)

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "Hi"}}

	StreamAgentRun(context.Background(), w, core, "thread-1", messages, zerolog.Nop())

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	events := parseSSEEvents(w.Body.String())
	if len(events) == 0 {
		t.Fatal("expected SSE events, got none")
	}

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
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Error("expected data: [DONE] sentinel")
	}
}

func TestStreamAgentRun_ToolCallAndResult(t *testing.T) {
	agent := makeToolCallAgent(t)
	core := newTestCore(t, agent)

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "Show me all products"}}

	StreamAgentRun(context.Background(), w, core, "thread-2", messages, zerolog.Nop())

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
}

func TestStreamAgentRun_HITLInterrupt(t *testing.T) {
	agent := makeHITLAgent(t)
	core := newTestCore(t, agent)

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "Insert a new user"}}

	StreamAgentRun(context.Background(), w, core, "thread-hitl", messages, zerolog.Nop())

	events := parseSSEEvents(w.Body.String())

	var hasInterrupt bool
	var hasToolCall bool

	for _, evt := range events {
		if ti, ok := evt["tool_interrupt"]; ok {
			hasInterrupt = true
			interrupt := ti.(map[string]any)
			if interrupt["toolCallId"] != "call_write_001" {
				t.Errorf("expected toolCallId call_write_001, got %v", interrupt["toolCallId"])
			}
			if interrupt["toolName"] != "write_database" {
				t.Errorf("expected toolName write_database, got %v", interrupt["toolName"])
			}
			if interrupt["thread_id"] != "thread-hitl" {
				t.Errorf("expected thread_id thread-hitl, got %v", interrupt["thread_id"])
			}
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
	}

	if !hasInterrupt {
		t.Error("expected tool_interrupt event")
	}
	if !hasToolCall {
		t.Error("expected tool_call delta for the original write_database call")
	}

	// Verify interrupt was stored
	pending := core.Interrupts.Get("thread-hitl")
	if pending == nil {
		t.Fatal("expected pending interrupt to be stored")
	}
	if pending.ConfirmationCallID != "confirm_001" {
		t.Errorf("expected confirmation call ID confirm_001, got %s", pending.ConfirmationCallID)
	}
	if pending.ToolCallID != "call_write_001" {
		t.Errorf("expected tool call ID call_write_001, got %s", pending.ToolCallID)
	}
	if pending.ToolName != "write_database" {
		t.Errorf("expected tool name write_database, got %s", pending.ToolName)
	}
}

func TestStreamAgentRun_StreamingTimestamps(t *testing.T) {
	partials := []string{"tok1", "tok2", "tok3", "tok4", "tok5"}
	agent := makeTextAgent(t, partials, "tok1tok2tok3tok4tok5")
	core := newTestCore(t, agent)

	w := httptest.NewRecorder()
	start := time.Now()
	StreamAgentRun(context.Background(), w, core, "thread-ts",
		[]types.ChatMessage{{Role: "user", Content: "test"}}, zerolog.Nop())
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
	if elapsed > 5*time.Second {
		t.Errorf("streaming took too long (%v), possible buffering issue", elapsed)
	}
}

func TestStreamResumeRun(t *testing.T) {
	// Make an agent that returns text on the resume call (confirmation response triggers re-run)
	resumeAgent := makeTextAgent(t, []string{"Done! ", "Record inserted."}, "Done! Record inserted.")
	core := newTestCore(t, resumeAgent)

	// Pre-create the session
	if err := core.SessionManager.GetOrCreate(context.Background(), "thread-resume"); err != nil {
		t.Fatal(err)
	}

	pending := &PendingInterrupt{
		ConfirmationCallID: "confirm_r1",
		ToolCallID:         "call_r1",
		ToolName:           "write_database",
		Prompt:             "Approve write?",
		Details:            map[string]any{"table": "users"},
	}

	w := httptest.NewRecorder()
	StreamResumeRun(context.Background(), w, core, "thread-resume", pending, true, zerolog.Nop())

	events := parseSSEEvents(w.Body.String())

	var textDeltas []string
	var hasStopFinish bool
	var hasToolCall bool
	var hasToolCallsFinish bool

	for _, evt := range events {
		choices, ok := evt["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasToolCall = true
			tc := tcs[0].(map[string]any)
			if tc["id"] != "call_r1" {
				t.Errorf("expected synthetic tool call id call_r1, got %v", tc["id"])
			}
			fn := tc["function"].(map[string]any)
			if fn["name"] != "write_database" {
				t.Errorf("expected tool name write_database, got %v", fn["name"])
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
		t.Error("expected synthetic tool_call delta before tool result")
	}
	if !hasToolCallsFinish {
		t.Error("expected finish_reason=tool_calls before tool result")
	}
	if len(textDeltas) < 2 {
		t.Errorf("expected streaming text after resume, got %d: %v", len(textDeltas), textDeltas)
	}
	if !hasStopFinish {
		t.Error("expected finish_reason=stop after resume")
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Error("expected [DONE] sentinel")
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

func TestInterruptStore(t *testing.T) {
	store := NewInterruptStore()

	if got := store.Get("thread-1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}

	store.Set("thread-1", &PendingInterrupt{
		ConfirmationCallID: "c1",
		ToolCallID:         "t1",
		ToolName:           "write_database",
	})
	got := store.Get("thread-1")
	if got == nil {
		t.Fatal("expected pending interrupt")
	}
	if got.ToolCallID != "t1" {
		t.Errorf("expected t1, got %s", got.ToolCallID)
	}

	store.Clear("thread-1")
	if got := store.Get("thread-1"); got != nil {
		t.Errorf("expected nil after clear, got %v", got)
	}
}
