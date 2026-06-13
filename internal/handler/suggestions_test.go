package handler

import "testing"

func TestFilterSuggestion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"run the tests", "run the tests"},
		{"  run the tests  ", "run the tests"},
		{`"run the tests"`, "run the tests"},
		{`'commit this'`, "commit this"},
		{"", ""},
		{"nothing", ""},
		{"silence", ""},
		{"looks good thanks", ""},
		{"great job on that", ""},
		{"what about the API?", ""},
		{"Let me check that", ""},
		{"I'll do it", ""},
		{"Here's what I found", ""},
		{"a", "a"}, // single word allowed
		{"yes", "yes"},
		{"push it", "push it"},
		{"this is way too long of a suggestion that has more than twelve words in it definitely", ""},
	}

	for _, tt := range tests {
		got := filterSuggestion(tt.input)
		if got != tt.expected {
			t.Errorf("filterSuggestion(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
