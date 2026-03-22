// Package memory provides long-term memory storage backed by OpenSearch.
// Memories are scoped by (app_name, user_id) and support both text search
// and optional knn vector search for semantic retrieval.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentic/internal/config"
	"agentic/internal/rag"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

// Entry represents a memory document stored in OpenSearch.
type Entry struct {
	ID        string    `json:"id,omitempty"`
	AppName   string    `json:"app_name"`
	UserID    string    `json:"user_id"`
	Content   string    `json:"content"`
	Vector    []float64 `json:"vector,omitempty"`
	CreatedAt string    `json:"created_at,omitempty"`
	UpdatedAt string    `json:"updated_at,omitempty"`
}

// Service provides CRUD and search operations on long-term memories.
type Service struct {
	os     *opensearch.Client
	cfg    *config.Config
	logger zerolog.Logger
}

// NewService creates a new memory service.
func NewService(osClient *opensearch.Client, cfg *config.Config, logger zerolog.Logger) *Service {
	return &Service{
		os:     osClient,
		cfg:    cfg,
		logger: logger.With().Str("component", "memory").Logger(),
	}
}

// Add stores a new memory entry, optionally embedding it for semantic search.
func (s *Service) Add(ctx context.Context, appName, userID, content string) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	doc := map[string]any{
		"app_name":   appName,
		"user_id":    userID,
		"content":    content,
		"created_at": now,
		"updated_at": now,
	}

	// Embed if RAG config is available
	if vec, err := rag.EmbedQuery(ctx, s.cfg, content); err == nil {
		doc["vector"] = vec
	} else {
		s.logger.Debug().Err(err).Msg("embedding unavailable, storing without vector")
	}

	id, err := s.os.IndexDocument(ctx, opensearch.IndexMemories, "", doc)
	if err != nil {
		return "", fmt.Errorf("index memory: %w", err)
	}
	return id, nil
}

// Update replaces the content of an existing memory entry.
func (s *Service) Update(ctx context.Context, appName, userID, memoryID, content string) error {
	// Verify ownership
	if err := s.verifyOwnership(ctx, appName, userID, memoryID); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	doc := map[string]any{
		"content":    content,
		"updated_at": now,
	}

	if vec, err := rag.EmbedQuery(ctx, s.cfg, content); err == nil {
		doc["vector"] = vec
	}

	return s.os.UpdateDocument(ctx, opensearch.IndexMemories, memoryID, doc)
}

// Delete removes a memory entry by ID.
func (s *Service) Delete(ctx context.Context, appName, userID, memoryID string) error {
	if err := s.verifyOwnership(ctx, appName, userID, memoryID); err != nil {
		return err
	}
	return s.os.DeleteDocument(ctx, opensearch.IndexMemories, memoryID)
}

// Search finds relevant memories using vector similarity (if available) with
// text search fallback. Results are scoped to (appName, userID).
func (s *Service) Search(ctx context.Context, appName, userID, query string, count int) ([]Entry, error) {
	if count <= 0 {
		count = 5
	}
	if count > 50 {
		count = 50
	}

	filter := []map[string]any{
		{"term": map[string]any{"app_name": appName}},
		{"term": map[string]any{"user_id": userID}},
	}

	// Try vector search first
	if vec, err := rag.EmbedQuery(ctx, s.cfg, query); err == nil {
		q := map[string]any{
			"size": count,
			"query": map[string]any{
				"bool": map[string]any{
					"must": []any{
						map[string]any{
							"knn": map[string]any{
								"vector": map[string]any{
									"vector": vec,
									"k":      count,
								},
							},
						},
					},
					"filter": filter,
				},
			},
		}

		resp, err := s.os.Search(ctx, opensearch.IndexMemories, q)
		if err == nil && len(resp.Hits.Hits) > 0 {
			return s.hitsToEntries(resp), nil
		}
	}

	// Fallback: text search
	q := map[string]any{
		"size": count,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"match": map[string]any{
							"content": query,
						},
					},
				},
				"filter": filter,
			},
		},
		"sort": []map[string]any{{"_score": map[string]string{"order": "desc"}}},
	}

	resp, err := s.os.Search(ctx, opensearch.IndexMemories, q)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	return s.hitsToEntries(resp), nil
}

// List returns all memories for a user, ordered by most recently updated.
func (s *Service) List(ctx context.Context, appName, userID string, count int) ([]Entry, error) {
	if count <= 0 {
		count = 50
	}
	if count > 200 {
		count = 200
	}

	q := map[string]any{
		"size": count,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []map[string]any{
					{"term": map[string]any{"app_name": appName}},
					{"term": map[string]any{"user_id": userID}},
				},
			},
		},
		"sort": []map[string]any{{"updated_at": map[string]string{"order": "desc"}}},
	}

	resp, err := s.os.Search(ctx, opensearch.IndexMemories, q)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	return s.hitsToEntries(resp), nil
}

// verifyOwnership ensures the memory belongs to the given app+user.
func (s *Service) verifyOwnership(ctx context.Context, appName, userID, memoryID string) error {
	hit, err := s.os.GetDocument(ctx, opensearch.IndexMemories, memoryID)
	if err != nil {
		return fmt.Errorf("memory not found: %w", err)
	}

	var doc struct {
		AppName string `json:"app_name"`
		UserID  string `json:"user_id"`
	}
	if err := json.Unmarshal(hit.Source, &doc); err != nil {
		return fmt.Errorf("parse memory: %w", err)
	}
	if doc.AppName != appName || doc.UserID != userID {
		return fmt.Errorf("memory not found or access denied")
	}
	return nil
}

func (s *Service) hitsToEntries(resp *opensearch.SearchResponse) []Entry {
	entries := make([]Entry, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var e Entry
		if err := json.Unmarshal(hit.Source, &e); err != nil {
			continue
		}
		e.ID = hit.ID
		e.Vector = nil // don't return vectors to callers
		entries = append(entries, e)
	}
	return entries
}
