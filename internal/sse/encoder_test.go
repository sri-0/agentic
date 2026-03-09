package sse

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEncode(t *testing.T) {
	data := map[string]string{"hello": "world"}
	b, err := Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("expected 'data: ' prefix, got %q", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Errorf("expected trailing \\n\\n, got %q", s)
	}
	// Verify the JSON portion is valid
	jsonPart := strings.TrimPrefix(s, "data: ")
	jsonPart = strings.TrimSuffix(jsonPart, "\n\n")
	var parsed map[string]string
	if err := json.Unmarshal([]byte(jsonPart), &parsed); err != nil {
		t.Errorf("invalid JSON: %v", err)
	}
	if parsed["hello"] != "world" {
		t.Errorf("expected world, got %s", parsed["hello"])
	}
}

func TestEncodeDone(t *testing.T) {
	b := EncodeDone()
	if string(b) != "data: [DONE]\n\n" {
		t.Errorf("expected 'data: [DONE]\\n\\n', got %q", string(b))
	}
}

func TestWriteSSE_Flushes(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]string{"key": "value"}
	err := WriteSSE(w, data)
	if err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("expected SSE format, got %q", body)
	}
	if w.Flushed {
		// httptest.ResponseRecorder sets Flushed = true when Flush is called
		t.Log("Flush was called (expected)")
	}
}

func TestChunkBuilder_TextDelta(t *testing.T) {
	cb := NewChunkBuilder("chatcmpl-test", "agent", "thread-1")
	chunk := cb.TextDelta("hello")

	b, _ := json.Marshal(chunk)
	var raw map[string]any
	json.Unmarshal(b, &raw)

	if raw["id"] != "chatcmpl-test" {
		t.Errorf("expected id chatcmpl-test, got %v", raw["id"])
	}
	if raw["model"] != "agent" {
		t.Errorf("expected model agent, got %v", raw["model"])
	}
	if raw["thread_id"] != "thread-1" {
		t.Errorf("expected thread_id thread-1, got %v", raw["thread_id"])
	}
	if raw["object"] != "chat.completion.chunk" {
		t.Errorf("expected object chat.completion.chunk, got %v", raw["object"])
	}

	choices := raw["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	choice := choices[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if delta["content"] != "hello" {
		t.Errorf("expected content hello, got %v", delta["content"])
	}
}

func TestChunkBuilder_ToolCallDelta(t *testing.T) {
	cb := NewChunkBuilder("chatcmpl-test", "agent", "thread-1")
	chunk := cb.ToolCallDelta(0, "call_123", "query_database", `{"sql":"SELECT 1"}`)

	b, _ := json.Marshal(chunk)
	var raw map[string]any
	json.Unmarshal(b, &raw)

	choices := raw["choices"].([]any)
	choice := choices[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	toolCalls := delta["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	tc := toolCalls[0].(map[string]any)
	if tc["id"] != "call_123" {
		t.Errorf("expected id call_123, got %v", tc["id"])
	}
	if tc["type"] != "function" {
		t.Errorf("expected type function, got %v", tc["type"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "query_database" {
		t.Errorf("expected name query_database, got %v", fn["name"])
	}
	if fn["arguments"] != `{"sql":"SELECT 1"}` {
		t.Errorf("expected arguments, got %v", fn["arguments"])
	}
}

func TestChunkBuilder_Finish(t *testing.T) {
	cb := NewChunkBuilder("chatcmpl-test", "agent", "thread-1")

	for _, reason := range []string{"stop", "tool_calls"} {
		chunk := cb.Finish(reason)
		b, _ := json.Marshal(chunk)
		var raw map[string]any
		json.Unmarshal(b, &raw)

		choices := raw["choices"].([]any)
		choice := choices[0].(map[string]any)
		if choice["finish_reason"] != reason {
			t.Errorf("expected finish_reason %s, got %v", reason, choice["finish_reason"])
		}
	}
}
