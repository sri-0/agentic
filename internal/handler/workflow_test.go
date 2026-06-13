package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentic/agents/basic"
	"agentic/agents/deepresearch"
	"agentic/agents/triage"
	"agentic/internal/agent"
	"agentic/internal/config"
	"agentic/internal/hitl"
	"agentic/internal/rag"
	"agentic/internal/tools"

	"github.com/rs/zerolog"
	"google.golang.org/adk/session"
)

// newFakeUpstreamMulti creates a fake OpenAI-compatible upstream that handles
// both streaming and non-streaming requests, returning a simple text response.
func newFakeUpstreamMulti(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Analysis complete.\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-test",
			"model": req["model"],
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Analysis complete.",
				},
			}},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 5,
				"total_tokens":      15,
			},
		})
	}))
}

func makeTestConfig(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()
	t.Setenv("TEST_API_KEY", "test-key")
	return &config.Config{
		AppName: "test_app",
		Models: &config.ModelsConfig{
			Providers: []config.Provider{{
				ID:        "test",
				Name:      "Test",
				BaseURL:   upstreamURL,
				APIKeyEnv: "TEST_API_KEY",
				Models:    []config.Model{{ID: "openai/gpt-4o", Type: config.ModelTypeLLM}},
			}},
		},
	}
}

// parseSSEEvents extracts text parts, event types, and checks for [DONE] from an SSE response body.
type sseResult struct {
	textParts       []string            // choices[0].delta.content
	agentEventTexts []string            // agent_event text_delta content
	agentStarts     []string            // agent_progress agent_start agent names
	agentDones      []string            // agent_progress agent_done agent names
	hasAgentProg    bool
	hasAgentEvent   bool
	hasToolResult   bool
	hasDone         bool
	rawLines        []string
}

func parseSSE(body string) sseResult {
	var res sseResult
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		res.rawLines = append(res.rawLines, line)

		if line == "data: [DONE]" {
			res.hasDone = true
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		var evt map[string]any
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		if ap, ok := evt["agent_progress"].(map[string]any); ok {
			res.hasAgentProg = true
			if phase, _ := ap["phase"].(string); phase == "agent_start" {
				if name, _ := ap["agent"].(string); name != "" {
					res.agentStarts = append(res.agentStarts, name)
				}
			}
			if phase, _ := ap["phase"].(string); phase == "agent_done" {
				if name, _ := ap["agent"].(string); name != "" {
					res.agentDones = append(res.agentDones, name)
				}
			}
		}
		if ae, ok := evt["agent_event"].(map[string]any); ok {
			res.hasAgentEvent = true
			if aeType, _ := ae["type"].(string); aeType == "text_delta" {
				if content, _ := ae["content"].(string); content != "" {
					res.agentEventTexts = append(res.agentEventTexts, content)
				}
			}
		}
		if _, ok := evt["tool_result"]; ok {
			res.hasToolResult = true
		}

		if choices, ok := evt["choices"].([]any); ok && len(choices) > 0 {
			choice := choices[0].(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if c, ok := delta["content"].(string); ok && c != "" {
					res.textParts = append(res.textParts, c)
				}
			}
		}
	}
	return res
}

func sendChatRequest(t *testing.T, handler http.HandlerFunc, model, message, threadID string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"model":     model,
		"messages":  []map[string]string{{"role": "user", "content": message}},
		"stream":    true,
		"thread_id": threadID,
	}
	bodyJSON, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBuffer(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// deepResearchAgentCfg returns the standard test config for the deep research agent.
func deepResearchAgentCfg() config.AgentConfig {
	return config.AgentConfig{
		ID:          "deep-research",
		Type:        "deep-research",
		Name:        "deep_research_pipeline",
		OutputAgent: "report_generator",
		SubAgents: []config.AgentConfig{
			{Name: "research_planner", Model: "openai/gpt-4o", Provider: "test", OutputKey: "research_plan", SystemPrompt: "Plan research for the query."},
			{Name: "document_analyst", Model: "openai/gpt-4o", Provider: "test", OutputKey: "analysis_findings", SystemPrompt: "Analyze: {current_document}"},
			{Name: "data_analyst", Model: "openai/gpt-4o", Provider: "test", OutputKey: "database_results", SystemPrompt: "Query data for: {current_document}", Tools: []string{"query_research_db"}},
			{Name: "findings_summarizer", Model: "openai/gpt-4o", Provider: "test", OutputKey: "current_findings", SystemPrompt: "Summarize: {analysis_findings} {database_results}"},
			{Name: "gap_analyst", Model: "openai/gpt-4o", Provider: "test", OutputKey: "research_gaps", SystemPrompt: "Identify gaps: {research_plan} vs {all_findings}"},
			{Name: "report_generator", Model: "openai/gpt-4o", Provider: "test", OutputKey: "draft_report", SystemPrompt: "Report on: {all_findings} gaps: {research_gaps} feedback: {critic_feedback}"},
			{Name: "report_critic", Model: "openai/gpt-4o", Provider: "test", OutputKey: "critic_feedback", SystemPrompt: "Review: {draft_report}. If good say APPROVED."},
		},
	}
}

// buildDeepResearchHandler creates a fully wired handler for deep research tests.
func buildDeepResearchHandler(t *testing.T, cfg *config.Config) http.HandlerFunc {
	t.Helper()
	agentCfg := deepResearchAgentCfg()
	cfg.Agents = &config.AgentsConfig{Agents: []config.AgentConfig{agentCfg}}

	deps := tools.Deps{RAGClient: rag.NewClient(nil, nil)}
	rootAgent, err := deepresearch.NewAgent(cfg, &agentCfg, deps)
	if err != nil {
		t.Fatal(err)
	}

	core, err := agent.NewCoreWithAgent(cfg, &agentCfg, rootAgent, hitl.NewInMemoryStore(), session.InMemoryService(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	core.OutputAgent = agentCfg.OutputAgent

	reg := agent.NewRegistry()
	reg.Register("deep-research", core)
	return Chat(reg, cfg, nil, nil, nil, zerolog.Nop())
}

// ─── Basic Agent ─────────────────────────────────────────────────────────────

func TestWorkflow_BasicAgent(t *testing.T) {
	upstream := newFakeUpstreamMulti(t)
	defer upstream.Close()

	cfg := makeTestConfig(t, upstream.URL)
	agentCfg := config.AgentConfig{
		ID:           "basic-agent",
		Type:         "basic",
		Name:         "basic_agent",
		Model:        "openai/gpt-4o",
		Provider:     "test",
		SystemPrompt: "You are a helpful assistant.",
		Tools:        []string{"calculate"},
	}
	cfg.Agents = &config.AgentsConfig{Agents: []config.AgentConfig{agentCfg}}

	deps := tools.Deps{RAGClient: rag.NewClient(nil, nil)}
	rootAgent, err := basic.NewAgent(cfg, &agentCfg, deps)
	if err != nil {
		t.Fatal(err)
	}

	core, err := agent.NewCoreWithAgent(cfg, &agentCfg, rootAgent, hitl.NewInMemoryStore(), session.InMemoryService(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	reg := agent.NewRegistry()
	reg.Register("basic-agent", core)
	handler := Chat(reg, cfg, nil, nil, nil, zerolog.Nop())

	w := sendChatRequest(t, handler, "basic-agent", "What is 2+2?", "basic-t1")

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	res := parseSSE(w.Body.String())
	if !res.hasDone {
		t.Error("missing [DONE]")
	}
	if !res.hasAgentProg {
		t.Error("missing agent_progress event")
	}
	if len(res.textParts) == 0 {
		t.Error("expected text content in response")
	}

	combined := strings.Join(res.textParts, "")
	t.Logf("basic agent response: %q", combined)
}

// ─── Deep Research Agent ─────────────────────────────────────────────────────

func TestWorkflow_DeepResearchAgent(t *testing.T) {
	t.Skip("requires OpenSearch with seeded data (rag_retrieval code agent calls opensearch directly)")
	upstream := newFakeUpstreamMulti(t)
	defer upstream.Close()

	cfg := makeTestConfig(t, upstream.URL)
	handler := buildDeepResearchHandler(t, cfg)

	w := sendChatRequest(t, handler, "deep-research", "What is our Q4 2024 business performance?", "dr-t1")

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	res := parseSSE(w.Body.String())

	if !res.hasDone {
		t.Error("missing [DONE]")
	}
	if !res.hasAgentProg {
		t.Error("missing agent_progress event")
	}

	// With OutputAgent="report_generator", final report text goes to choices.delta.content
	// and intermediate agent text goes to agent_event
	if len(res.textParts) == 0 {
		t.Fatal("expected text content from report_generator in choices.delta.content")
	}

	// Code agent and intermediate LLM text should be in agent_event channel
	allAgentText := strings.Join(res.agentEventTexts, "")
	if !strings.Contains(allAgentText, "Retrieved") {
		t.Error("expected RAG retrieval progress in agent_event")
	}
	if !strings.Contains(allAgentText, "Fetched document") {
		t.Error("expected document fetch progress in agent_event")
	}
	if !strings.Contains(allAgentText, "Accumulated findings") {
		t.Error("expected findings accumulation progress in agent_event")
	}

	// Agent lifecycle events should fire
	if len(res.agentStarts) < 3 {
		t.Errorf("expected agent_start events for multiple agents, got %d: %v", len(res.agentStarts), res.agentStarts)
	}

	t.Logf("deep research: %d content events, %d agent_event texts, agents started: %v",
		len(res.textParts), len(res.agentEventTexts), res.agentStarts)
}

func TestWorkflow_DeepResearch_MultipleDocuments(t *testing.T) {
	t.Skip("requires OpenSearch with seeded data")
	upstream := newFakeUpstreamMulti(t)
	defer upstream.Close()

	cfg := makeTestConfig(t, upstream.URL)
	handler := buildDeepResearchHandler(t, cfg)

	w := sendChatRequest(t, handler, "deep-research", "market analysis", "dr-multi")

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	res := parseSSE(w.Body.String())

	// Code agent output goes to agent_event channel
	allAgentText := strings.Join(res.agentEventTexts, "")

	// RAG returns 5 mock documents — verify all were processed
	fetchCount := strings.Count(allAgentText, "Fetched document")
	if fetchCount < 3 {
		t.Errorf("expected at least 3 document fetches in agent_event, got %d", fetchCount)
	}

	accumCount := strings.Count(allAgentText, "Accumulated findings for document")
	if accumCount < 3 {
		t.Errorf("expected at least 3 accumulations in agent_event, got %d", accumCount)
	}

	if !strings.Contains(allAgentText, "documents processed") {
		t.Error("loop should have completed all documents")
	}

	t.Logf("processed %d documents, %d accumulations", fetchCount, accumCount)
}

func TestWorkflow_DeepResearch_ThreadPersistence(t *testing.T) {
	t.Skip("requires OpenSearch with seeded data")
	upstream := newFakeUpstreamMulti(t)
	defer upstream.Close()

	cfg := makeTestConfig(t, upstream.URL)
	handler := buildDeepResearchHandler(t, cfg)

	// First request
	w1 := sendChatRequest(t, handler, "deep-research", "Q4 performance", "persist-thread")
	if w1.Code != 200 {
		t.Fatalf("first request: expected 200, got %d", w1.Code)
	}
	res1 := parseSSE(w1.Body.String())
	if !res1.hasDone {
		t.Error("first request: missing [DONE]")
	}

	// Second request on same thread
	w2 := sendChatRequest(t, handler, "deep-research", "Tell me more about revenue", "persist-thread")
	if w2.Code != 200 {
		t.Fatalf("second request: expected 200, got %d", w2.Code)
	}
	res2 := parseSSE(w2.Body.String())
	if !res2.hasDone {
		t.Error("second request: missing [DONE]")
	}
	if len(res2.textParts) == 0 {
		t.Error("second request: expected text content")
	}
}

// ─── Triage Agent ────────────────────────────────────────────────────────────

func TestWorkflow_TriageAgent(t *testing.T) {
	upstream := newFakeUpstreamMulti(t)
	defer upstream.Close()

	cfg := makeTestConfig(t, upstream.URL)
	agentCfg := config.AgentConfig{
		ID:   "triage-agent",
		Type: "triage",
		Name: "triage_pipeline",
		SubAgents: []config.AgentConfig{
			{
				Name:         "issue_extractor",
				Model:        "openai/gpt-4o",
				Provider:     "test",
				OutputKey:    "extracted_issue",
				SystemPrompt: "Extract issue details from the incident report.",
			},
			{
				Name:         "keyword_analyst",
				Model:        "openai/gpt-4o",
				Provider:     "test",
				OutputKey:    "keyword_analysis",
				SystemPrompt: "Analyze keywords: {extracted_issue}",
				Tools:        []string{"classify_incident"},
			},
			{
				Name:         "incident_researcher",
				Model:        "openai/gpt-4o",
				Provider:     "test",
				OutputKey:    "incident_research",
				SystemPrompt: "Research past incidents: {extracted_issue}",
				Tools:        []string{"get_incident_context", "opensearch_retrieve"},
			},
			{
				Name:         "severity_classifier",
				Model:        "openai/gpt-4o",
				Provider:     "test",
				OutputKey:    "severity_classification",
				SystemPrompt: "Classify severity: {extracted_issue}",
				Tools:        []string{"classify_incident"},
			},
			{
				Name:         "report_agent",
				Model:        "openai/gpt-4o",
				Provider:     "test",
				OutputKey:    "triage_report",
				SystemPrompt: "Triage report: {keyword_analysis} {incident_research} {severity_classification}",
				Tools:        []string{"trigger_alert"},
			},
		},
	}
	cfg.Agents = &config.AgentsConfig{Agents: []config.AgentConfig{agentCfg}}

	deps := tools.Deps{RAGClient: rag.NewClient(nil, nil)}
	rootAgent, err := triage.NewAgent(cfg, &agentCfg, deps)
	if err != nil {
		t.Fatal(err)
	}

	core, err := agent.NewCoreWithAgent(cfg, &agentCfg, rootAgent, hitl.NewInMemoryStore(), session.InMemoryService(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	reg := agent.NewRegistry()
	reg.Register("triage-agent", core)
	handler := Chat(reg, cfg, nil, nil, nil, zerolog.Nop())

	w := sendChatRequest(t, handler, "triage-agent", "Critical: Database primary node is down, all write operations failing since 14:30 UTC. Multiple services affected.", "triage-t1")

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	res := parseSSE(w.Body.String())

	if !res.hasDone {
		t.Error("missing [DONE]")
	}
	if !res.hasAgentProg {
		t.Error("missing agent_progress event")
	}
	if len(res.textParts) == 0 {
		t.Fatal("expected text content from triage pipeline")
	}

	combined := strings.Join(res.textParts, "")
	// All sub-agents should have produced output
	if !strings.Contains(combined, "Analysis complete") {
		t.Error("expected LLM sub-agent text in triage output")
	}

	t.Logf("triage output length: %d chars, %d SSE text events", len(combined), len(res.textParts))
}

// ─── Multi-Agent Registry ────────────────────────────────────────────────────

func TestWorkflow_MultiAgentRegistry(t *testing.T) {
	upstream := newFakeUpstreamMulti(t)
	defer upstream.Close()

	cfg := makeTestConfig(t, upstream.URL)

	basicCfg := config.AgentConfig{
		ID: "basic-agent", Type: "basic", Name: "basic_agent",
		Model: "openai/gpt-4o", Provider: "test", SystemPrompt: "You are helpful.",
		Tools: []string{"calculate"},
	}
	triageCfg := config.AgentConfig{
		ID: "triage-agent", Type: "triage", Name: "triage_pipeline",
		SubAgents: []config.AgentConfig{
			{Name: "issue_extractor", Model: "openai/gpt-4o", Provider: "test", OutputKey: "extracted_issue", SystemPrompt: "Extract."},
			{Name: "keyword_analyst", Model: "openai/gpt-4o", Provider: "test", OutputKey: "keyword_analysis", SystemPrompt: "Keywords: {extracted_issue}", Tools: []string{"classify_incident"}},
			{Name: "incident_researcher", Model: "openai/gpt-4o", Provider: "test", OutputKey: "incident_research", SystemPrompt: "Research: {extracted_issue}", Tools: []string{"get_incident_context"}},
			{Name: "severity_classifier", Model: "openai/gpt-4o", Provider: "test", OutputKey: "severity_classification", SystemPrompt: "Severity: {extracted_issue}", Tools: []string{"classify_incident"}},
			{Name: "report_agent", Model: "openai/gpt-4o", Provider: "test", OutputKey: "triage_report", SystemPrompt: "Report: {keyword_analysis} {incident_research} {severity_classification}", Tools: []string{"trigger_alert"}},
		},
	}
	cfg.Agents = &config.AgentsConfig{Agents: []config.AgentConfig{basicCfg, triageCfg}}

	deps := tools.Deps{RAGClient: rag.NewClient(nil, nil)}
	reg := agent.NewRegistry()

	// Register basic agent
	ba, err := basic.NewAgent(cfg, &basicCfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := agent.NewCoreWithAgent(cfg, &basicCfg, ba, hitl.NewInMemoryStore(), session.InMemoryService(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("basic-agent", bc)

	// Register triage agent
	ta, err := triage.NewAgent(cfg, &triageCfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := agent.NewCoreWithAgent(cfg, &triageCfg, ta, hitl.NewInMemoryStore(), session.InMemoryService(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("triage-agent", tc)

	handler := Chat(reg, cfg, nil, nil, nil, zerolog.Nop())

	// Route to basic agent
	w1 := sendChatRequest(t, handler, "basic-agent", "hello", "multi-t1")
	if w1.Code != 200 {
		t.Fatalf("basic: expected 200, got %d", w1.Code)
	}
	r1 := parseSSE(w1.Body.String())
	if !r1.hasDone {
		t.Error("basic: missing [DONE]")
	}

	// Route to triage agent
	w2 := sendChatRequest(t, handler, "triage-agent", "server down", "multi-t2")
	if w2.Code != 200 {
		t.Fatalf("triage: expected 200, got %d", w2.Code)
	}
	r2 := parseSSE(w2.Body.String())
	if !r2.hasDone {
		t.Error("triage: missing [DONE]")
	}

	// Unknown model should not crash (goes to proxy, which will fail without upstream, but shouldn't panic)
	w3 := sendChatRequest(t, handler, "nonexistent-model", "test", "multi-t3")
	if w3.Code == 200 {
		// Proxy would fail since there's no provider for this model — that's expected
		t.Log("proxy returned 200 (unexpected but not fatal)")
	}
}

// ─── SSE Format Verification ─────────────────────────────────────────────────

func TestWorkflow_SSEFormat_DeepResearch(t *testing.T) {
	t.Skip("requires OpenSearch with seeded data")
	upstream := newFakeUpstreamMulti(t)
	defer upstream.Close()

	cfg := makeTestConfig(t, upstream.URL)
	handler := buildDeepResearchHandler(t, cfg)

	w := sendChatRequest(t, handler, "deep-research", "business performance", "sse-fmt")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify SSE format compliance
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	bodyStr := w.Body.String()

	// Every SSE event should be "data: ...\n\n" format
	lines := strings.Split(bodyStr, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Errorf("non-SSE line in output: %q", line)
		}
	}

	// Must end with [DONE]
	if !strings.HasSuffix(strings.TrimSpace(bodyStr), "data: [DONE]") {
		t.Error("SSE stream should end with data: [DONE]")
	}

	// Parse and verify chunk structure
	res := parseSSE(bodyStr)
	for _, line := range res.rawLines {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Errorf("invalid JSON in SSE event: %s", data[:min(80, len(data))])
			continue
		}

		// Standard OpenAI chunks should have choices
		if choices, ok := chunk["choices"]; ok {
			choicesArr, ok := choices.([]any)
			if !ok || len(choicesArr) == 0 {
				t.Error("choices should be non-empty array")
			}
			// Verify chunk has required fields
			if _, ok := chunk["id"]; !ok {
				t.Error("chunk missing 'id' field")
			}
			if _, ok := chunk["model"]; !ok {
				t.Error("chunk missing 'model' field")
			}
		}
	}

	t.Logf("SSE format check passed: %d events, %d text parts", len(res.rawLines), len(res.textParts))
}
