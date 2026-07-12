package chat

import (
	"context"
	"fmt"
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

// SaveUserMessage persists a user turn. turn is the 0-based turn index the
// coordinator derived for this run (eventlog.NextTurn): when >= 0 the doc is
// keyed deterministically as {threadID}:{turn}:user — the same scheme the
// archiver uses for assistant messages — so a re-send upserts in place and the
// reloaded history id matches the live turn identity. A negative turn (legacy
// non-coordinated paths) falls back to a random doc id.
func (s *MessageSaver) SaveUserMessage(ctx context.Context, threadID, userID, content string, turn int) {
	if s.client == nil {
		return
	}
	docID := uuid.New().String()
	if turn >= 0 {
		docID = fmt.Sprintf("%s:%d:user", threadID, turn)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	doc := map[string]any{
		"thread_id":  threadID,
		"user_id":    userID, // required: GET /v1/threads/{id}/messages scopes by user_id
		"role":       "user",
		"content":    content,
		"created_at": now,
	}
	go func() {
		bgCtx := context.Background()
		// Ensure a thread doc exists so the conversation-history sidebar lists it
		// and the TitleHook (which reads the thread doc) can run. The frontend
		// generates thread_id client-side and never calls POST /v1/threads, so the
		// chat path must upsert the thread on the first user message. Create-if-
		// absent keyed by threadID is idempotent: it runs every user turn but only
		// creates once, and never clobbers a title the TitleHook later sets. The
		// initial "New Chat" title is treated as UNSET by agent.titleUnset(), so
		// title generation still fires.
		created, err := s.client.CreateDocumentIfAbsent(bgCtx, opensearch.IndexThreads, threadID, map[string]any{
			"user_id":    userID,
			"title":      "New Chat",
			"created_at": now,
			"updated_at": now,
		})
		if err != nil {
			s.logger.Warn().Err(err).Str("thread_id", threadID).Msg("failed to ensure thread doc")
		}
		if !created {
			// Thread already existed (a later turn): bump updated_at so the sidebar
			// re-sorts this thread to the top. On first creation updated_at==now
			// already, so only bump on subsequent turns.
			s.client.UpdateDocument(bgCtx, opensearch.IndexThreads, threadID, map[string]any{
				"updated_at": now,
			})
		}
		if _, err := s.client.IndexDocument(bgCtx, opensearch.IndexMessages, docID, doc); err != nil {
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
