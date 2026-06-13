package prompts

// CompactionData holds template variables for compaction prompts.
type CompactionData struct {
	CustomInstructions string
}

// SessionMemoryUpdateData holds template variables for the session memory update prompt.
type SessionMemoryUpdateData struct {
	CurrentNotes     string
	MaxSectionLength int
	SectionReminders string
}
