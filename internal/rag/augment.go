package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentic/internal/config"
	"agentic/internal/types"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

const defaultPrompt = "Use the following retrieved context to help answer the question. Cite sources by number when relevant.\n\n---\n{{context}}\n---\n\n{{query}}"

// AugmentMessages embeds the user query, performs KNN vector search against
// the embeddings index, and prepends retrieved context to the last user message.
// Falls back to text match if embedding fails. No-op if osClient is nil or messages is empty.
func AugmentMessages(ctx context.Context, cfg *config.Config, osClient *opensearch.Client, messages []types.ChatMessage, logger zerolog.Logger) []types.ChatMessage {
	if osClient == nil || len(messages) == 0 {
		return messages
	}

	ragCfg := cfg.RAG
	if ragCfg == nil {
		ragCfg = &config.RAGConfig{TopK: 5, Index: "embeddings"}
	}

	lastIdx := len(messages) - 1
	userQuery := messages[lastIdx].Content

	topK := ragCfg.TopK
	if topK <= 0 {
		topK = 5
	}

	index := ragCfg.Index
	if index == "" {
		index = opensearch.IndexEmbeddings
	}

	// Try vector search first: embed query then KNN
	hits, err := vectorSearch(ctx, cfg, osClient, userQuery, topK, index, logger)
	if err != nil {
		logger.Warn().Err(err).Msg("rag: vector search failed, falling back to text match")
		hits, err = textSearch(ctx, osClient, userQuery, topK, index)
		if err != nil {
			logger.Warn().Err(err).Msg("rag: text search also failed, proceeding without context")
			return messages
		}
	}

	if len(hits) == 0 {
		logger.Info().Str("query", userQuery).Msg("rag: no results found")
		return messages
	}

	contextStr := formatContext(hits)

	prompt := ragCfg.Prompt
	if prompt == "" {
		prompt = defaultPrompt
	}
	augmented := strings.ReplaceAll(prompt, "{{context}}", contextStr)
	augmented = strings.ReplaceAll(augmented, "{{query}}", userQuery)

	out := make([]types.ChatMessage, len(messages))
	copy(out, messages)
	out[lastIdx] = types.ChatMessage{Role: messages[lastIdx].Role, Content: augmented}

	logger.Info().Int("results", len(hits)).Msg("rag: context injected")
	return out
}

func vectorSearch(ctx context.Context, cfg *config.Config, osClient *opensearch.Client, query string, topK int, index string, logger zerolog.Logger) ([]opensearch.Hit, error) {
	vector, err := EmbedQuery(ctx, cfg, query)
	if err != nil {
		return nil, err
	}

	logger.Info().Int("dims", len(vector)).Msg("rag: query embedded")

	resp, err := osClient.KNNSearch(ctx, index, "vector", vector, topK, nil)
	if err != nil {
		return nil, fmt.Errorf("knn search: %w", err)
	}

	return resp.Hits.Hits, nil
}

func textSearch(ctx context.Context, osClient *opensearch.Client, query string, topK int, index string) ([]opensearch.Hit, error) {
	q := map[string]any{
		"size": topK,
		"query": map[string]any{
			"match": map[string]any{
				"text": query,
			},
		},
	}

	resp, err := osClient.Search(ctx, index, q)
	if err != nil {
		return nil, err
	}

	return resp.Hits.Hits, nil
}

func formatContext(hits []opensearch.Hit) string {
	var parts []string
	for i, hit := range hits {
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
		parts = append(parts, ref)
	}
	return strings.Join(parts, "\n\n")
}
