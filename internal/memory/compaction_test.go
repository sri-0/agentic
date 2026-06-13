package memory

import "testing"

func TestFormatCompactSummary(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "strips analysis and extracts summary",
			input: `<analysis>
Some thinking here
</analysis>

<summary>
1. Primary Request: Build a feature
2. Key Concepts: Go, ADK
</summary>`,
			expected: `1. Primary Request: Build a feature
2. Key Concepts: Go, ADK`,
		},
		{
			name:     "returns raw when no tags",
			input:    "Just a plain summary",
			expected: "Just a plain summary",
		},
		{
			name: "handles analysis without summary tags",
			input: `<analysis>
thinking
</analysis>

This is the summary content.`,
			expected: "This is the summary content.",
		},
		{
			name:     "handles empty input",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatCompactSummary(tt.input)
			if got != tt.expected {
				t.Errorf("formatCompactSummary() =\n%q\nwant\n%q", got, tt.expected)
			}
		})
	}
}

func TestAnalyzeSectionSizes(t *testing.T) {
	content := `# Title
Some short title

# Current State
This is a longer section with more content that describes the current state of affairs.

# Empty Section
`
	sections := analyzeSectionSizes(content)
	if len(sections) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(sections))
	}
	if _, ok := sections["# Title"]; !ok {
		t.Fatal("missing # Title section")
	}
	if _, ok := sections["# Current State"]; !ok {
		t.Fatal("missing # Current State section")
	}
}

func TestGenerateSectionReminders(t *testing.T) {
	// Under budget — no reminders
	reminder := generateSectionReminders("short content")
	if reminder != "" {
		t.Fatalf("expected empty reminder, got %q", reminder)
	}

	// Over budget — should warn
	longContent := ""
	for i := 0; i < 60000; i++ {
		longContent += "word "
	}
	reminder = generateSectionReminders(longContent)
	if reminder == "" {
		t.Fatal("expected budget warning for long content")
	}
}
