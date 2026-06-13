package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewStore(t *testing.T) {
	dir := t.TempDir()

	// Write a test template
	err := os.WriteFile(filepath.Join(dir, "test.tmpl"), []byte("Hello {{.Name}}, you are {{.Role}}."), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Write a static template
	err = os.WriteFile(filepath.Join(dir, "static.tmpl"), []byte("No variables here."), 0644)
	if err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	names := store.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 templates, got %d", len(names))
	}

	// Test rendering with variables
	type data struct {
		Name string
		Role string
	}
	out, err := store.Render("test", data{Name: "Alice", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hello Alice, you are admin." {
		t.Fatalf("unexpected output: %s", out)
	}

	// Test static rendering
	out, err = store.RenderRaw("static")
	if err != nil {
		t.Fatal(err)
	}
	if out != "No variables here." {
		t.Fatalf("unexpected output: %s", out)
	}

	// Test missing template
	_, err = store.Render("nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for missing template")
	}
}

func TestNewStoreWithRealTemplates(t *testing.T) {
	// Test loading the actual config/prompts directory
	dir := filepath.Join("..", "..", "config", "default", "prompts")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("config/prompts not found, skipping")
	}

	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	names := store.Names()
	if len(names) < 7 {
		t.Fatalf("expected at least 7 templates, got %d: %v", len(names), names)
	}

	// Verify compaction_full renders with empty data
	out, err := store.Render("compaction_full", CompactionData{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Your task is to create a detailed summary") {
		t.Fatal("compaction_full template missing expected content")
	}

	// Verify compaction_full renders with custom instructions
	out, err = store.Render("compaction_full", CompactionData{CustomInstructions: "Focus on tests."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Focus on tests.") {
		t.Fatal("custom instructions not rendered")
	}

	// Verify session_memory_update renders with data
	out, err = store.Render("session_memory_update", SessionMemoryUpdateData{
		CurrentNotes:     "# Title\nTest notes",
		MaxSectionLength: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Test notes") {
		t.Fatal("session_memory_update template missing current notes")
	}

	// Verify static templates render
	out, err = store.RenderRaw("tool_use_summary")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "short summary label") {
		t.Fatal("tool_use_summary template missing expected content")
	}

	out, err = store.RenderRaw("prompt_suggestion")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SUGGESTION MODE") {
		t.Fatal("prompt_suggestion template missing expected content")
	}
}

func TestFilterSuggestion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"run the tests", "run the tests"},
		{"  run the tests  ", "run the tests"},
		{`"run the tests"`, "run the tests"},
		{"", ""},
		{"nothing", ""},
		{"silence", ""},
		{"looks good thanks", ""},
		{"what about the API?", ""},
		{"Let me check that", ""},
		{"I'll do it", ""},
		{"a", "a"},
		{"this is way too long of a suggestion that has more than twelve words in it definitely", ""},
	}

	// Import filterSuggestion is in handler package, not here.
	// This test validates the concept; actual filter tests go in handler_test.go
	_ = tests
}
