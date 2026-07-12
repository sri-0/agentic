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
	"agentic/internal/hitl"

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
	return newTestCoreWithOutput(t, customAgent, "")
}

// newTestCoreWithOutput creates a Core with OutputAgent set for routing tests.
func newTestCoreWithOutput(t *testing.T, customAgent adkagent.Agent, outputAgent string) *Core {
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
		Interrupts:     hitl.NewInMemoryStore(),
		AgentID:        "test-agent",
		OutputAgent:    outputAgent,
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

	StreamAgentRun(context.Background(), w, core, "thread-1", "", messages, nil, zerolog.Nop())

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

	StreamAgentRun(context.Background(), w, core, "thread-2", "", messages, nil, zerolog.Nop())

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

// makeTodoWriteAgent yields a todowrite tool call + response carrying a todos
// snapshot, then a short final text.
func makeTodoWriteAgent(t *testing.T) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "todo_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				callEvt := session.NewEvent(ctx.InvocationID())
				callEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_todo",
								Name: "todowrite",
								Args: map[string]any{"todos": []any{
									map[string]any{"content": "Research topic", "status": "in_progress", "priority": "high"},
									map[string]any{"content": "Write report", "status": "pending"},
								}},
							},
						}},
					},
				}
				callEvt.Author = "todo_agent"
				if !yield(callEvt, nil) {
					return
				}

				respEvt := session.NewEvent(ctx.InvocationID())
				respEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionResponse: &genai.FunctionResponse{
								ID:   "call_todo",
								Name: "todowrite",
								Response: map[string]any{
									"status": "written",
									"todos": []any{
										map[string]any{"content": "Research topic", "status": "in_progress", "priority": "high"},
										map[string]any{"content": "Write report", "status": "pending"},
									},
								},
							},
						}},
					},
				}
				respEvt.Author = "todo_agent"
				if !yield(respEvt, nil) {
					return
				}

				finalEvt := session.NewEvent(ctx.InvocationID())
				finalEvt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Done."}},
					},
					TurnComplete: true,
				}
				finalEvt.Author = "todo_agent"
				yield(finalEvt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStreamAgentRun_TodoWriteTaskList(t *testing.T) {
	agent := makeTodoWriteAgent(t)
	core := newTestCore(t, agent)

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "Plan the work"}}

	StreamAgentRun(context.Background(), w, core, "thread-todo", "", messages, nil, zerolog.Nop())

	events := parseSSEEvents(w.Body.String())

	var taskListVal map[string]any
	var hasToolResult bool
	for _, evt := range events {
		if _, ok := evt["tool_result"]; ok {
			hasToolResult = true
		}
		agui, ok := evt["ag_ui"].(map[string]any)
		if !ok {
			continue
		}
		if agui["name"] == "task_list" {
			taskListVal, _ = agui["value"].(map[string]any)
		}
	}

	if taskListVal == nil {
		t.Fatal("expected a task_list CUSTOM event")
	}
	tasks, _ := taskListVal["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d: %v", len(tasks), tasks)
	}
	t0 := tasks[0].(map[string]any)
	if t0["title"] != "Research topic" || t0["status"] != "in_progress" || t0["priority"] != "high" {
		t.Errorf("unexpected task[0]: %v", t0)
	}
	if t0["id"] != "0" {
		t.Errorf("expected task[0] id '0', got %v", t0["id"])
	}
	if !hasToolResult {
		t.Error("expected a tool_result event so call/result pairing stays intact")
	}
}

func TestStreamAgentRun_HITLInterrupt(t *testing.T) {
	agent := makeHITLAgent(t)
	core := newTestCore(t, agent)

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "Insert a new user"}}

	StreamAgentRun(context.Background(), w, core, "thread-hitl", "", messages, nil, zerolog.Nop())

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
	pending, _ := core.Interrupts.Get("thread-hitl")
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
	StreamAgentRun(context.Background(), w, core, "thread-ts", "",
		[]types.ChatMessage{{Role: "user", Content: "test"}}, nil, zerolog.Nop())
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

	pending := &hitl.PendingInterrupt{
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

// makeMultiAgentWorkflow creates a sequential workflow with two agents:
// an "analyst" that emits text, then a "reporter" (output agent) that emits final text.
func makeMultiAgentWorkflow(t *testing.T) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "workflow_root",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				// Analyst emits partial text
				for _, text := range []string{"Analyzing ", "data..."} {
					evt := session.NewEvent(ctx.InvocationID())
					evt.LLMResponse = model.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{Text: text}},
						},
						Partial: true,
					}
					evt.Author = "analyst"
					if !yield(evt, nil) {
						return
					}
				}
				// Analyst non-partial
				evt := session.NewEvent(ctx.InvocationID())
				evt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Analyzing data..."}},
					},
				}
				evt.Author = "analyst"
				if !yield(evt, nil) {
					return
				}

				// Reporter (output agent) emits partial text
				for _, text := range []string{"Final ", "report."} {
					evt := session.NewEvent(ctx.InvocationID())
					evt.LLMResponse = model.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{Text: text}},
						},
						Partial: true,
					}
					evt.Author = "reporter"
					if !yield(evt, nil) {
						return
					}
				}
				// Reporter non-partial
				evt = session.NewEvent(ctx.InvocationID())
				evt.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Final report."}},
					},
					TurnComplete: true,
				}
				evt.Author = "reporter"
				yield(evt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// makeSubAgentToolWorkflow has a sub-agent (analyst) make a tool call/result
// plus reasoning, then the output agent (reporter) emits final text.
func makeSubAgentToolWorkflow(t *testing.T) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "workflow_root",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				// Analyst reasoning (thought part)
				rev := session.NewEvent(ctx.InvocationID())
				rev.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Let me think.", Thought: true}},
					},
					Partial: true,
				}
				rev.Author = "analyst"
				if !yield(rev, nil) {
					return
				}
				// Analyst tool call
				cev := session.NewEvent(ctx.InvocationID())
				cev.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionCall: &genai.FunctionCall{
								ID:   "sub_call_1",
								Name: "web_search",
								Args: map[string]any{"query": "x"},
							},
						}},
					},
				}
				cev.Author = "analyst"
				if !yield(cev, nil) {
					return
				}
				// Analyst tool result
				rrev := session.NewEvent(ctx.InvocationID())
				rrev.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionResponse: &genai.FunctionResponse{
								ID:       "sub_call_1",
								Name:     "web_search",
								Response: map[string]any{"results": 3},
							},
						}},
					},
				}
				rrev.Author = "analyst"
				if !yield(rrev, nil) {
					return
				}
				// Reporter (output) final text
				fev := session.NewEvent(ctx.InvocationID())
				fev.LLMResponse = model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: "Report."}},
					},
					TurnComplete: true,
				}
				fev.Author = "reporter"
				yield(fev, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStreamAgentRun_SubAgentToolAttribution(t *testing.T) {
	workflow := makeSubAgentToolWorkflow(t)
	core := newTestCoreWithOutput(t, workflow, "reporter")

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "go"}}

	StreamAgentRun(context.Background(), w, core, "thread-subtool", "", messages, nil, zerolog.Nop())

	events := parseSSEEvents(w.Body.String())

	var subToolCall, subToolResult, subReasoning bool
	var openAIToolCall bool
	for _, evt := range events {
		if ae, ok := evt["agent_event"].(map[string]any); ok {
			switch ae["type"] {
			case "tool_call":
				if ae["agent"] == "analyst" && ae["tool_name"] == "web_search" && ae["tool_call_id"] == "sub_call_1" {
					subToolCall = true
				}
			case "tool_result":
				if ae["agent"] == "analyst" && ae["tool_call_id"] == "sub_call_1" {
					subToolResult = true
				}
			case "reasoning":
				if ae["agent"] == "analyst" && ae["reasoning_content"] == "Let me think." {
					subReasoning = true
				}
			}
		}
		if choices, ok := evt["choices"].([]any); ok && len(choices) > 0 {
			choice := choices[0].(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
					openAIToolCall = true
				}
			}
		}
	}

	if !subToolCall {
		t.Error("expected attributed sub-agent tool_call agent_event")
	}
	if !subToolResult {
		t.Error("expected attributed sub-agent tool_result agent_event")
	}
	if !subReasoning {
		t.Error("expected sub-agent reasoning with reasoning_content")
	}
	if openAIToolCall {
		t.Error("sub-agent tool call must NOT use the OpenAI tool_calls channel")
	}
}

func TestStreamAgentRun_AgentEventRouting(t *testing.T) {
	workflow := makeMultiAgentWorkflow(t)
	core := newTestCoreWithOutput(t, workflow, "reporter")

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "research something"}}

	StreamAgentRun(context.Background(), w, core, "thread-routing", "", messages, nil, zerolog.Nop())

	events := parseSSEEvents(w.Body.String())

	var contentDeltas []string   // choices[0].delta.content
	var agentEvents []string     // agent_event text_delta content
	var agentTextDone []string   // agent_event text_done
	var agentStarts []string     // agent_progress agent_start
	var agentDones []string      // agent_progress agent_done

	for _, evt := range events {
		// Check agent_event
		if ae, ok := evt["agent_event"].(map[string]any); ok {
			aeType := ae["type"].(string)
			if aeType == "text_delta" {
				agentEvents = append(agentEvents, ae["content"].(string))
			}
			if aeType == "text_done" {
				agentTextDone = append(agentTextDone, ae["agent"].(string))
			}
			continue
		}
		// Check agent_progress
		if ap, ok := evt["agent_progress"].(map[string]any); ok {
			phase := ap["phase"].(string)
			if phase == "agent_start" {
				agentStarts = append(agentStarts, ap["agent"].(string))
			}
			if phase == "agent_done" {
				agentDones = append(agentDones, ap["agent"].(string))
			}
			continue
		}
		// Check choices (content deltas)
		choices, ok := evt["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			contentDeltas = append(contentDeltas, content)
		}
	}

	// Analyst text should go to agent_event, NOT to content deltas
	if len(agentEvents) < 2 {
		t.Errorf("expected at least 2 agent_event text_deltas for analyst, got %d: %v", len(agentEvents), agentEvents)
	}
	// Reporter text should go to content deltas, NOT to agent_event
	if len(contentDeltas) < 2 {
		t.Errorf("expected at least 2 content deltas for reporter, got %d: %v", len(contentDeltas), contentDeltas)
	}
	// Analyst text_done should fire
	if len(agentTextDone) == 0 {
		t.Error("expected agent_event text_done for analyst")
	}
	// Agent lifecycle events
	if len(agentStarts) < 2 {
		t.Errorf("expected agent_start for both analyst and reporter, got %v", agentStarts)
	}
	if len(agentDones) < 2 {
		t.Errorf("expected agent_done for both analyst and reporter, got %v", agentDones)
	}
	// Verify content deltas contain reporter text, not analyst text
	combined := strings.Join(contentDeltas, "")
	if !strings.Contains(combined, "Final") {
		t.Errorf("expected reporter text in content deltas, got: %s", combined)
	}
	if strings.Contains(combined, "Analyzing") {
		t.Error("analyst text should NOT appear in content deltas")
	}
}

func TestStreamAgentRun_FlatAgentNoRouting(t *testing.T) {
	// Flat agent (no OutputAgent) — all text goes to content deltas, no agent_events
	agent := makeTextAgent(t, []string{"Hello", " world"}, "Hello world")
	core := newTestCore(t, agent) // OutputAgent is ""

	w := httptest.NewRecorder()
	messages := []types.ChatMessage{{Role: "user", Content: "Hi"}}

	StreamAgentRun(context.Background(), w, core, "thread-flat", "", messages, nil, zerolog.Nop())

	events := parseSSEEvents(w.Body.String())

	var contentDeltas []string
	var hasAgentEvent bool

	for _, evt := range events {
		if _, ok := evt["agent_event"]; ok {
			hasAgentEvent = true
		}
		choices, ok := evt["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			contentDeltas = append(contentDeltas, content)
		}
	}

	if hasAgentEvent {
		t.Error("flat agent should not emit agent_event")
	}
	if len(contentDeltas) < 2 {
		t.Errorf("expected text in content deltas, got %d", len(contentDeltas))
	}
}

func TestInterruptStore(t *testing.T) {
	store := hitl.NewInMemoryStore()

	if got, _ := store.Get("thread-1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}

	_ = store.Set("thread-1", &hitl.PendingInterrupt{
		ConfirmationCallID: "c1",
		ToolCallID:         "t1",
		ToolName:           "write_database",
	})
	got, _ := store.Get("thread-1")
	if got == nil {
		t.Fatal("expected pending interrupt")
	}
	if got.ToolCallID != "t1" {
		t.Errorf("expected t1, got %s", got.ToolCallID)
	}

	_ = store.Clear("thread-1")
	if got, _ := store.Get("thread-1"); got != nil {
		t.Errorf("expected nil after clear, got %v", got)
	}
}
