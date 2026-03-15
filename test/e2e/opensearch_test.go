package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"agentic/internal/chat"
	"agentic/internal/config"
	"agentic/internal/handler"
	"agentic/internal/rag"
	"agentic/internal/types"
	"agentic/pkg/db/opensearch"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

var (
	osClient *opensearch.Client
	logger   = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()
)

func TestMain(m *testing.M) {
	url := os.Getenv("OPENSEARCH_URL")
	if url == "" {
		url = "http://localhost:9200"
	}

	osClient = opensearch.New(opensearch.Config{URL: url}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := osClient.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: OpenSearch not reachable at %s: %v\n", url, err)
		os.Exit(0)
	}

	if err := opensearch.EnsureIndices(ctx, osClient); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: could not ensure indices: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// ── OpenSearch client ───────────────────────────────────────────────────────

func TestPing(t *testing.T) {
	if err := osClient.Ping(context.Background()); err != nil {
		t.Fatalf("ping failed: %v", err)
	}
}

func TestCreateAndDeleteIndex(t *testing.T) {
	ctx := context.Background()
	idx := "test_lifecycle"
	defer osClient.DeleteIndex(ctx, idx)

	mapping := json.RawMessage(`{"mappings":{"properties":{"name":{"type":"keyword"}}}}`)
	if err := osClient.CreateIndex(ctx, idx, mapping); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Idempotent
	if err := osClient.CreateIndex(ctx, idx, mapping); err != nil {
		t.Fatalf("create idempotent: %v", err)
	}
	if err := osClient.DeleteIndex(ctx, idx); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// ── RAG search endpoint ────────────────────────────────────────────────────

func setupRouter() *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/v1/rag/search", handler.RAGSearch(osClient, logger)).Methods("POST")
	r.HandleFunc("/v1/prompts", handler.PromptsList(osClient, logger)).Methods("GET")
	r.HandleFunc("/v1/prompts", handler.PromptsCreate(osClient, logger)).Methods("POST")
	r.HandleFunc("/v1/prompts/{id}", handler.PromptsGet(osClient, logger)).Methods("GET")
	r.HandleFunc("/v1/prompts/{id}", handler.PromptsUpdate(osClient, logger)).Methods("PUT")
	r.HandleFunc("/v1/prompts/{id}", handler.PromptsDelete(osClient, logger)).Methods("DELETE")
	return r
}

func TestRAGSearch_TextMatch(t *testing.T) {
	cleanup := seedEmbeddings(t)
	defer cleanup()

	router := setupRouter()

	body, _ := json.Marshal(map[string]any{
		"query": "revenue enterprise growth",
		"top_k": 3,
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/rag/search", bytes.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	results := resp["results"].([]any)
	if len(results) == 0 {
		t.Fatal("expected results for 'revenue enterprise growth'")
	}
	t.Logf("text search returned %d results (total: %v)", len(results), resp["total"])

	// First result should be the Q4 performance report
	first := results[0].(map[string]any)
	doc := first["document"].(map[string]any)
	if title, ok := doc["title"].(string); ok {
		t.Logf("top result: %s (score: %v)", title, first["score"])
	}
}

func TestRAGSearch_WithFilters(t *testing.T) {
	cleanup := seedEmbeddings(t)
	defer cleanup()

	router := setupRouter()

	body, _ := json.Marshal(map[string]any{
		"query":   "analysis report",
		"top_k":   10,
		"filters": map[string]string{"classification": "confidential"},
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/rag/search", bytes.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	results := resp["results"].([]any)
	t.Logf("filtered search (confidential): %d results", len(results))

	// All results should be confidential
	for _, r := range results {
		doc := r.(map[string]any)["document"].(map[string]any)
		if doc["classification"] != "confidential" {
			t.Errorf("expected classification=confidential, got %v", doc["classification"])
		}
	}
}

func TestRAGSearch_ProjectFilter(t *testing.T) {
	cleanup := seedEmbeddings(t)
	defer cleanup()

	router := setupRouter()

	body, _ := json.Marshal(map[string]any{
		"query":   "data pipeline",
		"top_k":   5,
		"filters": map[string]string{"project": "beta-project"},
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/rag/search", bytes.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	results := resp["results"].([]any)
	t.Logf("project-filtered search: %d results", len(results))
	for _, r := range results {
		doc := r.(map[string]any)["document"].(map[string]any)
		if doc["project"] != "beta-project" {
			t.Errorf("expected project=beta-project, got %v", doc["project"])
		}
	}
}

// ── RAG augmentation (internal/rag) ─────────────────────────────────────────

func TestAugmentMessages(t *testing.T) {
	cleanup := seedEmbeddings(t)
	defer cleanup()

	messages := []types.ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "What was our revenue performance in Q4?"},
	}

	augmented := rag.AugmentMessages(context.Background(), &config.Config{}, osClient, messages, logger)

	if len(augmented) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(augmented))
	}

	// System message unchanged
	if augmented[0].Content != messages[0].Content {
		t.Error("system message was modified")
	}

	// User message should now contain RAG context
	if augmented[1].Content == messages[1].Content {
		t.Fatal("user message was not augmented with RAG context")
	}

	t.Logf("augmented message length: %d chars (was %d)", len(augmented[1].Content), len(messages[1].Content))

	// Should contain citation markers
	if !containsStr(augmented[1].Content, "[1]") {
		t.Error("augmented message missing citation markers")
	}
	if !containsStr(augmented[1].Content, "revenue") {
		t.Error("augmented context should contain relevant content about revenue")
	}
}

func TestAugmentMessages_NilClient(t *testing.T) {
	messages := []types.ChatMessage{
		{Role: "user", Content: "test query"},
	}

	// Should be a no-op, not panic
	result := rag.AugmentMessages(context.Background(), &config.Config{}, nil, messages, logger)
	if result[0].Content != "test query" {
		t.Error("nil client should return messages unchanged")
	}
}

// ── Prompt template application (internal/chat) ─────────────────────────────

func TestApplyPromptTemplate(t *testing.T) {
	_, cleanup := seedPrompts(t)
	defer cleanup()

	messages := []types.ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Summarize Q4 performance"},
	}

	result := chat.ApplyPromptTemplate(context.Background(), osClient, "prompt-analyst", messages, logger)

	if len(result) != 3 {
		t.Fatalf("expected 3 messages (system + prompt + user), got %d", len(result))
	}

	// Original system message first
	if result[0].Role != "system" || result[0].Content != "You are a helpful assistant." {
		t.Error("first message should be original system message")
	}

	// Injected prompt template second
	if result[1].Role != "system" {
		t.Errorf("prompt template should be system role, got %s", result[1].Role)
	}
	if !containsStr(result[1].Content, "senior business analyst") {
		t.Errorf("prompt template content mismatch: %s", result[1].Content)
	}

	// User message last
	if result[2].Role != "user" || result[2].Content != "Summarize Q4 performance" {
		t.Error("user message should be preserved as last")
	}
}

func TestApplyPromptTemplate_NilClient(t *testing.T) {
	messages := []types.ChatMessage{
		{Role: "user", Content: "test"},
	}
	result := chat.ApplyPromptTemplate(context.Background(), nil, "some-id", messages, logger)
	if len(result) != 1 {
		t.Error("nil client should return messages unchanged")
	}
}

func TestApplyPromptTemplate_InvalidID(t *testing.T) {
	messages := []types.ChatMessage{
		{Role: "user", Content: "test"},
	}
	result := chat.ApplyPromptTemplate(context.Background(), osClient, "nonexistent-id", messages, logger)
	if len(result) != 1 {
		t.Error("invalid prompt_id should return messages unchanged")
	}
}

func TestApplyPromptTemplate_NoSystemMessage(t *testing.T) {
	_, cleanup := seedPrompts(t)
	defer cleanup()

	messages := []types.ChatMessage{
		{Role: "user", Content: "What's the roadmap?"},
	}

	result := chat.ApplyPromptTemplate(context.Background(), osClient, "prompt-researcher", messages, logger)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}

	// Prompt inserted before user message
	if result[0].Role != "system" {
		t.Error("prompt should be inserted as system message")
	}
	if result[1].Role != "user" {
		t.Error("user message should follow prompt")
	}
}

// ── Combined RAG + prompt template ──────────────────────────────────────────

func TestRAGWithPromptTemplate(t *testing.T) {
	cleanupEmbed := seedEmbeddings(t)
	defer cleanupEmbed()
	_, cleanupPrompts := seedPrompts(t)
	defer cleanupPrompts()

	messages := []types.ChatMessage{
		{Role: "user", Content: "What are our competitive advantages?"},
	}

	// Apply prompt first
	messages = chat.ApplyPromptTemplate(context.Background(), osClient, "prompt-researcher", messages, logger)

	// Then RAG augmentation
	messages = rag.AugmentMessages(context.Background(), &config.Config{}, osClient, messages, logger)

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	// System message is the prompt template
	if !containsStr(messages[0].Content, "research assistant") {
		t.Error("first message should be the researcher prompt template")
	}

	// User message should be augmented with context
	if !containsStr(messages[1].Content, "[1]") {
		t.Error("user message should have RAG citation markers")
	}
	if !containsStr(messages[1].Content, "competitive") {
		t.Error("RAG context should contain competitive analysis content")
	}

	t.Logf("combined flow: %d messages, user msg %d chars", len(messages), len(messages[1].Content))
}

// ── Prompts CRUD endpoints ──────────────────────────────────────────────────

func TestPromptsCRUD(t *testing.T) {
	ctx := context.Background()
	router := setupRouter()

	// Create
	body, _ := json.Marshal(map[string]any{
		"name":        "crud-test-prompt",
		"description": "A test prompt for CRUD testing",
		"template":    "You are a {{role}}. Help with {{task}}.",
		"variables":   []string{"role", "task"},
		"tags":        []string{"test", "crud"},
		"version":     1,
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/prompts", bytes.NewReader(body)))

	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body: %s", w.Code, w.Body.String())
	}

	var created types.Prompt
	json.NewDecoder(w.Body).Decode(&created)
	promptID := created.ID
	t.Logf("created prompt: %s", promptID)

	if created.Name != "crud-test-prompt" {
		t.Errorf("name: got %q", created.Name)
	}
	if created.CreatedAt == "" {
		t.Error("created_at should be set")
	}

	osClient.Refresh(ctx, opensearch.IndexPrompts)

	// Get
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts/"+promptID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get: status %d", w.Code)
	}

	var fetched types.Prompt
	json.NewDecoder(w.Body).Decode(&fetched)
	if fetched.Template != "You are a {{role}}. Help with {{task}}." {
		t.Errorf("template mismatch: %q", fetched.Template)
	}
	if len(fetched.Variables) != 2 {
		t.Errorf("expected 2 variables, got %d", len(fetched.Variables))
	}

	// Update
	updateBody, _ := json.Marshal(map[string]any{
		"description": "Updated description for CRUD test",
		"version":     2,
	})
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("PUT", "/v1/prompts/"+promptID, bytes.NewReader(updateBody)))
	if w.Code != http.StatusOK {
		t.Fatalf("update: status %d, body: %s", w.Code, w.Body.String())
	}

	osClient.Refresh(ctx, opensearch.IndexPrompts)

	// Verify update
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts/"+promptID, nil))
	var updated types.Prompt
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.Version != 2 {
		t.Errorf("version: expected 2, got %d", updated.Version)
	}

	// List all
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: status %d", w.Code)
	}
	var listResp map[string]any
	json.NewDecoder(w.Body).Decode(&listResp)
	t.Logf("list: total=%v", listResp["total"])

	// List by tag
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts?tag=crud", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list by tag: status %d", w.Code)
	}
	json.NewDecoder(w.Body).Decode(&listResp)
	prompts := listResp["prompts"].([]any)
	if len(prompts) == 0 {
		t.Error("expected at least 1 prompt with tag=crud")
	}

	// Search
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts?q=CRUD", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("search: status %d", w.Code)
	}

	// Delete
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("DELETE", "/v1/prompts/"+promptID, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", w.Code)
	}

	// Verify deleted
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts/"+promptID, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("get after delete: expected 404, got %d", w.Code)
	}
}

func TestPromptsList_WithSeededData(t *testing.T) {
	_, cleanup := seedPrompts(t)
	defer cleanup()

	router := setupRouter()

	// List all
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: status %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	total := int(resp["total"].(float64))
	if total < 3 {
		t.Errorf("expected at least 3 seeded prompts, got %d", total)
	}

	prompts := resp["prompts"].([]any)
	t.Logf("listed %d prompts", len(prompts))
	for _, p := range prompts {
		pm := p.(map[string]any)
		t.Logf("  - %s: %s", pm["name"], pm["description"])
	}

	// Filter by tag
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts?tag=research", nil))
	json.NewDecoder(w.Body).Decode(&resp)
	prompts = resp["prompts"].([]any)
	if len(prompts) == 0 {
		t.Error("expected at least 1 prompt with tag=research")
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && contains(s, substr))
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
