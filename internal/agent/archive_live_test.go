package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"agentic/internal/eventlog"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

// TestArchiveMessagesMapping_AcceptsStructuredParts is a live integration test
// (skipped unless OPENSEARCH_TEST_URL is set) that locks the fix for the
// assistant-persistence bug: the `messages` index mapping must store the
// projected AI-SDK `parts` array (text/reasoning/dynamic-tool/data-agent-*) as
// an unindexed object, not as `text`. With the old `{"type":"text"}` mapping,
// OpenSearch rejected any document whose `parts` was a structured array with a
// mapper_parsing_exception, silently dropping every assistant message.
//
// The test uses a throwaway index (created with the production MessagesMapping
// via CreateIndex) so it never touches the shared `messages` index. It indexes
// the exact multi-part shape the archiver produces for a swarm turn and asserts
// the round-trip preserves both text and sub-agent parts.
func TestArchiveMessagesMapping_AcceptsStructuredParts(t *testing.T) {
	url := os.Getenv("OPENSEARCH_TEST_URL")
	if url == "" {
		t.Skip("set OPENSEARCH_TEST_URL to run the live messages-mapping test")
	}
	logger := zerolog.Nop()
	client := opensearch.New(opensearch.Config{URL: url}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	idx := "messages_maptest_go"
	_ = client.DeleteIndex(ctx, idx)
	if err := client.CreateIndex(ctx, idx, opensearch.MessagesMapping); err != nil {
		t.Fatalf("create index: %v", err)
	}
	defer client.DeleteIndex(ctx, idx)

	// The exact multi-part assistant message a swarm turn projects: a text part
	// plus a sub-agent data-agent-step part (the object that broke the old
	// text-typed mapping).
	msg := eventlog.ProjectedMessage{
		Role:    "assistant",
		Content: "HELLO",
		Parts: []eventlog.Part{
			{Type: eventlog.PartText, Text: "HELLO"},
			{Type: eventlog.PartAgentStep, ID: "basic_agent-1",
				Data: map[string]any{"agent": "basic_agent", "step": 1, "status": "started"}},
		},
	}
	partsJSON, _ := json.Marshal(msg.Parts)
	doc := map[string]any{
		"thread_id":  "t-maptest",
		"user_id":    "u1",
		"role":       msg.Role,
		"content":    msg.Content,
		"parts":      json.RawMessage(partsJSON),
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := client.IndexDocument(ctx, idx, "", doc); err != nil {
		t.Fatalf("index assistant message with structured parts failed (mapping regression?): %v", err)
	}
	_ = client.Refresh(ctx, idx)

	resp, err := client.Search(ctx, idx, map[string]any{
		"query": map[string]any{"term": map[string]any{"thread_id": "t-maptest"}},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Hits.Hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(resp.Hits.Hits))
	}
	var got struct {
		Role  string          `json:"role"`
		Parts []eventlog.Part `json:"parts"`
	}
	if err := json.Unmarshal(resp.Hits.Hits[0].Source, &got); err != nil {
		t.Fatalf("unmarshal source: %v", err)
	}
	if got.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", got.Role)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("parts round-trip dropped data: got %d parts, want 2", len(got.Parts))
	}
	if got.Parts[0].Type != eventlog.PartText || got.Parts[1].Type != eventlog.PartAgentStep {
		t.Fatalf("parts order/type wrong: %+v", got.Parts)
	}
}
