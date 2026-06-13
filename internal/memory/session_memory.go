// Package memory provides enhanced memory orchestration including session memory
// (short-term structured notes in Valkey) and context compaction services.
package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	internalagents "agentic/internal/agents"
	"agentic/internal/prompts"
	"agentic/internal/types"

	pkgvalkey "agentic/pkg/db/valkey"

	"github.com/rs/zerolog"
	"google.golang.org/adk/session"
)

const (
	sessionNotesPrefix = "session_notes:"
	sessionNotesTTL    = 7 * 24 * time.Hour // 7 days
	maxSectionLength   = 2000
	maxTotalTokens     = 12000
)

// SessionMemory manages structured session notes stored in Valkey,
// keyed by userId:threadId.
type SessionMemory struct {
	valkey     *pkgvalkey.Valkey
	prompts    *prompts.Store
	internal   *internalagents.Registry
	sessionSvc session.Service
	logger     zerolog.Logger
}

// NewSessionMemory creates a new session memory service.
func NewSessionMemory(
	valkey *pkgvalkey.Valkey,
	prompts *prompts.Store,
	internal *internalagents.Registry,
	sessionSvc session.Service,
	logger zerolog.Logger,
) *SessionMemory {
	return &SessionMemory{
		valkey:     valkey,
		prompts:    prompts,
		internal:   internal,
		sessionSvc: sessionSvc,
		logger:     logger.With().Str("component", "session_memory").Logger(),
	}
}

func sessionNotesKey(userID, threadID string) string {
	return sessionNotesPrefix + userID + ":" + threadID
}

// Get retrieves the current session notes for a user+thread.
// Returns empty string if no notes exist.
func (m *SessionMemory) Get(ctx context.Context, userID, threadID string) (string, error) {
	key := sessionNotesKey(userID, threadID)
	val, err := m.valkey.Get(ctx, key)
	if err != nil {
		return "", nil // key not found is not an error
	}
	return val, nil
}

// Initialize creates the initial session notes from the template.
func (m *SessionMemory) Initialize(ctx context.Context, userID, threadID string) error {
	key := sessionNotesKey(userID, threadID)

	// Check if already exists
	if val, _ := m.valkey.Get(ctx, key); val != "" {
		return nil
	}

	tmpl, err := m.prompts.RenderRaw("session_memory_template")
	if err != nil {
		return fmt.Errorf("rendering session memory template: %w", err)
	}

	return m.valkey.Set(ctx, key, tmpl, sessionNotesTTL)
}

// Update runs the session memory agent to update notes based on recent messages.
func (m *SessionMemory) Update(ctx context.Context, userID, threadID string, messages []types.ChatMessage) error {
	key := sessionNotesKey(userID, threadID)

	// Get current notes
	currentNotes, _ := m.valkey.Get(ctx, key)
	if currentNotes == "" {
		// Initialize first
		if err := m.Initialize(ctx, userID, threadID); err != nil {
			return err
		}
		currentNotes, _ = m.valkey.Get(ctx, key)
	}

	// Build the update prompt
	sectionReminders := generateSectionReminders(currentNotes)
	updatePrompt, err := m.prompts.Render("session_memory_update", prompts.SessionMemoryUpdateData{
		CurrentNotes:     currentNotes,
		MaxSectionLength: maxSectionLength,
		SectionReminders: sectionReminders,
	})
	if err != nil {
		return fmt.Errorf("rendering session memory update prompt: %w", err)
	}

	// Format recent messages as context
	var sb strings.Builder
	for _, msg := range messages {
		fmt.Fprintf(&sb, "[%s]: %s\n\n", msg.Role, msg.Content)
	}
	sb.WriteString("\n---\n\n")
	sb.WriteString(updatePrompt)

	// Run the session memory agent
	output, err := m.internal.Run(ctx, "session_memory", m.sessionSvc, userID, sb.String())
	if err != nil {
		m.logger.Error().Err(err).Str("thread_id", threadID).Msg("session memory agent failed")
		return err
	}

	if output == "" {
		return nil
	}

	// Store updated notes
	if err := m.valkey.Set(ctx, key, output, sessionNotesTTL); err != nil {
		return fmt.Errorf("storing session notes: %w", err)
	}

	m.logger.Debug().Str("thread_id", threadID).Int("notes_len", len(output)).Msg("session notes updated")
	return nil
}

// generateSectionReminders analyzes section sizes and returns warnings for oversized sections.
func generateSectionReminders(content string) string {
	if content == "" {
		return ""
	}

	sections := analyzeSectionSizes(content)
	totalTokens := roughTokenCount(content)

	overBudget := totalTokens > maxTotalTokens
	var oversized []string
	for section, tokens := range sections {
		if tokens > maxSectionLength {
			oversized = append(oversized, fmt.Sprintf("- %q is ~%d tokens (limit: %d)", section, tokens, maxSectionLength))
		}
	}

	if len(oversized) == 0 && !overBudget {
		return ""
	}

	var sb strings.Builder
	if overBudget {
		fmt.Fprintf(&sb, "\nCRITICAL: The session memory file is currently ~%d tokens, which exceeds the maximum of %d tokens. You MUST condense the file to fit within this budget.", totalTokens, maxTotalTokens)
	}
	if len(oversized) > 0 {
		sb.WriteString("\nOversized sections to condense:\n")
		sb.WriteString(strings.Join(oversized, "\n"))
	}
	return sb.String()
}

func analyzeSectionSizes(content string) map[string]int {
	sections := make(map[string]int)
	lines := strings.Split(content, "\n")
	var currentSection string
	var currentContent []string

	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			if currentSection != "" && len(currentContent) > 0 {
				sections[currentSection] = roughTokenCount(strings.Join(currentContent, "\n"))
			}
			currentSection = line
			currentContent = nil
		} else {
			currentContent = append(currentContent, line)
		}
	}
	if currentSection != "" && len(currentContent) > 0 {
		sections[currentSection] = roughTokenCount(strings.Join(currentContent, "\n"))
	}
	return sections
}

// roughTokenCount approximates token count as len/4 (common heuristic).
func roughTokenCount(s string) int {
	return len(s) / 4
}
