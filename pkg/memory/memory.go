// Package memory provides long-term memory storage backed by OpenSearch.
// Memories are scoped by (app_name, user_id) and support both text search
// and optional knn vector search for semantic retrieval.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"agentic/internal/config"
	"agentic/internal/rag"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

// dedupScoreThreshold is the minimum knn score (Lucene cosinesimil, which maps
// cosine similarity to (1+cos)/2 in [0,1]) at which an incoming memory is
// considered a near-duplicate of an existing one and skipped. ~0.97 catches
// re-phrasings of the same fact (e.g. "Favorite programming language: Rust"
// stored twice) without collapsing genuinely distinct facts.
const dedupScoreThreshold = 0.97

// ErrDuplicateMemory is returned by Add when the content is a near-duplicate of
// an existing memory for the same user; the caller may treat this as a no-op.
var ErrDuplicateMemory = fmt.Errorf("near-duplicate memory already exists")

// ErrJunkMemory is returned by Add when the content is empty or a contentless
// negative/unknown fact (e.g. "Work: NONE") that should never be persisted.
var ErrJunkMemory = fmt.Errorf("memory content is empty or a negative/unknown fact")

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
//
// Two guards run before persisting:
//  1. Junk rejection: empty or contentless negative/unknown facts (e.g.
//     "Work: NONE", "unknown", "N/A") are dropped — these pollute recall and
//     can even win a targeted query. Returns ErrJunkMemory.
//  2. Dedup: if a near-identical memory already exists for this user (knn
//     score >= dedupScoreThreshold, or exact normalized-text match), the write
//     is skipped so a single fact can't fragment into duplicates that crowd out
//     distinct facts in kNN top-k. Returns ErrDuplicateMemory with the existing ID.
func (s *Service) Add(ctx context.Context, appName, userID, content string) (string, error) {
	content = strings.TrimSpace(content)
	if isJunkContent(content) {
		s.logger.Debug().Str("content", content).Msg("rejecting junk/negative memory")
		return "", ErrJunkMemory
	}

	// Embed once; reuse the vector for both dedup detection and storage.
	vec, embErr := rag.EmbedQuery(ctx, s.cfg, content)
	if embErr != nil {
		s.logger.Debug().Err(embErr).Msg("embedding unavailable, storing without vector")
	}

	// Dedup: skip if a near-duplicate already exists for this user.
	if existingID, err := s.findDuplicate(ctx, appName, userID, content, vec); err == nil && existingID != "" {
		s.logger.Debug().Str("content", content).Str("existing_id", existingID).Msg("skipping near-duplicate memory")
		return existingID, ErrDuplicateMemory
	}

	now := time.Now().UTC().Format(time.RFC3339)
	doc := map[string]any{
		"app_name":   appName,
		"user_id":    userID,
		"content":    content,
		"created_at": now,
		"updated_at": now,
	}
	if embErr == nil {
		doc["vector"] = vec
	}

	id, err := s.os.IndexDocument(ctx, opensearch.IndexMemories, "", doc)
	if err != nil {
		return "", fmt.Errorf("index memory: %w", err)
	}
	return id, nil
}

// junkContentRe matches contentless negative/unknown facts whose only
// substantive token is a placeholder like NONE/UNKNOWN/N/A/NULL/NIL. This
// catches extractor output such as "Work: NONE" or "Favorite language - unknown"
// while leaving real facts ("Works at Prism Group") untouched.
var junkContentRe = regexp.MustCompile(`(?i)^(none|unknown|n/?a|null|nil|undefined)$`)

// isJunkContent reports whether content should never be persisted: empty, or a
// key:value / key - value pair whose value is a placeholder, or a bare placeholder.
func isJunkContent(content string) bool {
	if content == "" {
		return true
	}
	// Strip a leading "key:" or "key -" label and test the remaining value.
	value := content
	if idx := strings.IndexAny(content, ":-"); idx >= 0 {
		if v := strings.TrimSpace(content[idx+1:]); v != "" {
			value = v
		}
	}
	return junkContentRe.MatchString(strings.TrimSpace(value))
}

// findDuplicate returns the ID of an existing near-duplicate memory for the
// user, or "" if none. It prefers a vector kNN match (score >= threshold) and
// falls back to exact normalized-text equality when embeddings are unavailable.
func (s *Service) findDuplicate(ctx context.Context, appName, userID, content string, vec []float64) (string, error) {
	filter := []map[string]any{
		{"term": map[string]any{"app_name": appName}},
		{"term": map[string]any{"user_id": userID}},
	}

	if len(vec) > 0 {
		// Filter must live inside the knn clause (Lucene engine) — see Search().
		q := map[string]any{
			"size": 1,
			"query": map[string]any{
				"knn": map[string]any{
					"vector": map[string]any{
						"vector": vec,
						"k":      1,
						"filter": map[string]any{
							"bool": map[string]any{"filter": filter},
						},
					},
				},
			},
		}
		resp, err := s.os.Search(ctx, opensearch.IndexMemories, q)
		if err == nil && len(resp.Hits.Hits) > 0 && resp.Hits.Hits[0].Score >= dedupScoreThreshold {
			return resp.Hits.Hits[0].ID, nil
		}
	}

	// Fallback: exact normalized-text match (handles the no-embedding path).
	normalized := normalizeContent(content)
	q := map[string]any{
		"size": 1,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"match_phrase": map[string]any{"content": content}},
				},
				"filter": filter,
			},
		},
	}
	resp, err := s.os.Search(ctx, opensearch.IndexMemories, q)
	if err != nil {
		return "", err
	}
	for _, hit := range resp.Hits.Hits {
		var e Entry
		if json.Unmarshal(hit.Source, &e) == nil && normalizeContent(e.Content) == normalized {
			return hit.ID, nil
		}
	}
	return "", nil
}

// normalizeContent lowercases and collapses whitespace for exact-match dedup.
func normalizeContent(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
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

	// Try vector search first. The (app_name, user_id) scoping MUST live inside
	// the knn clause as its `filter` — with the Lucene engine, a knn query nested
	// under bool.must alongside a sibling bool.filter returns ZERO hits, which
	// silently degraded every semantic search to the lexical text fallback below
	// (the cause of combined-query recall missing distinct facts).
	if vec, err := rag.EmbedQuery(ctx, s.cfg, query); err == nil {
		q := map[string]any{
			"size": count,
			"query": map[string]any{
				"knn": map[string]any{
					"vector": map[string]any{
						"vector": vec,
						"k":      count,
						"filter": map[string]any{
							"bool": map[string]any{"filter": filter},
						},
					},
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
