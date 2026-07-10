package chat

import (
	"context"
	"time"

	"agentic/pkg/db/opensearch"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// MessageSaver persists thread messages to OpenSearch asynchronously.
// All methods are no-ops when the client is nil.
type MessageSaver struct {
	client *opensearch.Client
	logger zerolog.Logger
}

func NewMessageSaver(client *opensearch.Client, logger zerolog.Logger) *MessageSaver {
	return &MessageSaver{client: client, logger: logger}
}

func (s *MessageSaver) SaveUserMessage(ctx context.Context, threadID, content string) {
	if s.client == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	doc := map[string]any{
		"thread_id":  threadID,
		"role":       "user",
		"content":    content,
		"created_at": now,
	}
	go func() {
		if _, err := s.client.IndexDocument(context.Background(), opensearch.IndexMessages, uuid.New().String(), doc); err != nil {
			s.logger.Warn().Err(err).Str("thread_id", threadID).Msg("failed to save user message")
		}
	}()
}

func (s *MessageSaver) SaveAssistantMessage(ctx context.Context, threadID, content, model string) {
	s.SaveAssistantMessageWithParts(ctx, threadID, content, model, nil)
}

// SaveAssistantMessageWithParts persists an assistant message with its full
// AI-SDK parts payload (JSON) alongside the flattened content. parts is stored
// on the messages index `parts` field so GET /v1/threads/{id}/messages can
// rehydrate full messages (text/reasoning/tool/data-*), not text-only. content
// is still flattened for search/back-compat. A nil parts stores text-only.
func (s *MessageSaver) SaveAssistantMessageWithParts(ctx context.Context, threadID, content, model string, parts any) {
	if s.client == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	doc := map[string]any{
		"thread_id":  threadID,
		"role":       "assistant",
		"content":    content,
		"model":      model,
		"created_at": now,
	}
	if parts != nil {
		doc["parts"] = parts
	}
	go func() {
		bgCtx := context.Background()
		if _, err := s.client.IndexDocument(bgCtx, opensearch.IndexMessages, uuid.New().String(), doc); err != nil {
			s.logger.Warn().Err(err).Str("thread_id", threadID).Msg("failed to save assistant message")
		}
		// Bump thread updated_at
		s.client.UpdateDocument(bgCtx, opensearch.IndexThreads, threadID, map[string]any{
			"updated_at": now,
		})
	}()
}
