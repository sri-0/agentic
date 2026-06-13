package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	internalagents "agentic/internal/agents"
	"agentic/internal/prompts"
	"agentic/internal/types"

	"github.com/rs/zerolog"
	"google.golang.org/adk/session"
)

// CompactionService provides context window compaction by summarizing
// conversation history through an internal LLM agent.
type CompactionService struct {
	prompts    *prompts.Store
	internal   *internalagents.Registry
	sessionSvc session.Service
	logger     zerolog.Logger
}

// NewCompactionService creates a new compaction service.
func NewCompactionService(
	prompts *prompts.Store,
	internal *internalagents.Registry,
	sessionSvc session.Service,
	logger zerolog.Logger,
) *CompactionService {
	return &CompactionService{
		prompts:    prompts,
		internal:   internal,
		sessionSvc: sessionSvc,
		logger:     logger.With().Str("component", "compaction").Logger(),
	}
}

// CompactFull summarizes an entire conversation.
func (c *CompactionService) CompactFull(ctx context.Context, messages []types.ChatMessage, customInstructions string) (string, error) {
	return c.runCompaction(ctx, "compaction", messages, customInstructions)
}

// CompactPartial summarizes only the recent portion of a conversation.
func (c *CompactionService) CompactPartial(ctx context.Context, messages []types.ChatMessage, customInstructions string) (string, error) {
	return c.runCompaction(ctx, "compaction_partial", messages, customInstructions)
}

// CompactUpTo summarizes a conversation prefix for cache-prefix compaction.
func (c *CompactionService) CompactUpTo(ctx context.Context, messages []types.ChatMessage, customInstructions string) (string, error) {
	return c.runCompaction(ctx, "compaction_up_to", messages, customInstructions)
}

func (c *CompactionService) runCompaction(ctx context.Context, agentName string, messages []types.ChatMessage, customInstructions string) (string, error) {
	// Format messages as input
	var sb strings.Builder
	for _, msg := range messages {
		fmt.Fprintf(&sb, "[%s]: %s\n\n", msg.Role, msg.Content)
	}

	// If there are custom instructions and the agent system prompt doesn't have them
	// baked in, we need to use a dynamically-rendered prompt
	if customInstructions != "" {
		// Re-render the template with custom instructions
		templateName := strings.TrimPrefix(agentName, "compaction")
		if templateName == "" {
			templateName = "compaction_full"
		} else {
			templateName = "compaction" + templateName
		}
		_, err := c.prompts.Render(templateName, prompts.CompactionData{
			CustomInstructions: customInstructions,
		})
		if err != nil {
			c.logger.Warn().Err(err).Msg("failed to render custom compaction prompt, using default")
		}
	}

	output, err := c.internal.Run(ctx, agentName, c.sessionSvc, "system", sb.String())
	if err != nil {
		return "", fmt.Errorf("compaction agent %s failed: %w", agentName, err)
	}

	return formatCompactSummary(output), nil
}

// analysisRegex matches <analysis>...</analysis> blocks to strip them.
var analysisRegex = regexp.MustCompile(`(?s)<analysis>.*?</analysis>`)

// summaryRegex extracts content from <summary>...</summary> blocks.
var summaryRegex = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)

// formatCompactSummary strips analysis blocks and extracts the summary content.
func formatCompactSummary(raw string) string {
	// Strip analysis blocks
	cleaned := analysisRegex.ReplaceAllString(raw, "")

	// Extract summary content if wrapped in tags
	if matches := summaryRegex.FindStringSubmatch(cleaned); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}

	// Fallback: return cleaned content
	return strings.TrimSpace(cleaned)
}
