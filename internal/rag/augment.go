package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentic/internal/types"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

// AugmentMessages searches the embeddings index and prepends retrieved context
// to the last user message. No-op if osClient is nil or messages is empty.
func AugmentMessages(ctx context.Context, osClient *opensearch.Client, messages []types.ChatMessage, logger zerolog.Logger) []types.ChatMessage {
	if osClient == nil || len(messages) == 0 {
		return messages
	}

	lastIdx := len(messages) - 1
	userQuery := messages[lastIdx].Content

	query := map[string]any{
		"size": 5,
		"query": map[string]any{
			"match": map[string]any{
				"text": userQuery,
			},
		},
	}

	resp, err := osClient.Search(ctx, opensearch.IndexEmbeddings, query)
	if err != nil {
		logger.Warn().Err(err).Msg("RAG augmentation search failed, proceeding without context")
		return messages
	}

	if len(resp.Hits.Hits) == 0 {
		logger.Debug().Str("query", userQuery).Msg("RAG search returned no results")
		return messages
	}

	var contextParts []string
	for i, hit := range resp.Hits.Hits {
		var doc map[string]any
		if err := json.Unmarshal(hit.Source, &doc); err != nil {
			continue
		}
		title, _ := doc["title"].(string)
		text, _ := doc["text"].(string)
		source, _ := doc["source"].(string)

		ref := fmt.Sprintf("[%d] %s", i+1, text)
		if title != "" {
			ref = fmt.Sprintf("[%d] %s: %s", i+1, title, text)
		}
		if source != "" {
			ref += fmt.Sprintf(" (source: %s)", source)
		}
		contextParts = append(contextParts, ref)
	}

	augmented := fmt.Sprintf(
		"Use the following retrieved context to help answer the question. Cite sources by number when relevant.\n\n---\n%s\n---\n\n%s",
		strings.Join(contextParts, "\n\n"),
		userQuery,
	)

	// Copy slice to avoid mutating the original
	out := make([]types.ChatMessage, len(messages))
	copy(out, messages)
	out[lastIdx] = types.ChatMessage{Role: messages[lastIdx].Role, Content: augmented}

	logger.Debug().Int("context_docs", len(resp.Hits.Hits)).Msg("RAG context injected")
	return out
}
