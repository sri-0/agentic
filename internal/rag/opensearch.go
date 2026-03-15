package rag

import (
	"context"
	"encoding/json"
	"fmt"

	"agentic/internal/config"
	"agentic/pkg/db/opensearch"
)

// DocumentMetadata is the configurable schema for document metadata from OpenSearch.
type DocumentMetadata struct {
	DocumentID      string  `json:"document_id"`
	Source          string  `json:"source"`
	Author          string  `json:"author"`
	Date            string  `json:"date"`
	Classification  string  `json:"classification"`
	ConfidenceScore float64 `json:"confidence_score"`
}

// Document is a single document returned by the RAG system.
type Document struct {
	Metadata DocumentMetadata `json:"metadata"`
	Title    string           `json:"title"`
	Content  string           `json:"content"`
}

// Finding is an extracted insight with source references.
type Finding struct {
	Claim      string             `json:"claim"`
	Evidence   string             `json:"evidence"`
	SourceRefs []DocumentMetadata `json:"source_refs"`
	Confidence float64            `json:"confidence"`
}

// DatabaseResult holds data from a database query.
type DatabaseResult struct {
	Query    string           `json:"query"`
	Table    string           `json:"table"`
	Rows     []map[string]any `json:"rows"`
	RowCount int              `json:"row_count"`
}

// Citation is a numbered reference in a report.
type Citation struct {
	RefNumber int              `json:"ref_number"`
	Metadata  DocumentMetadata `json:"metadata"`
}

// ResearchReport is the final structured output.
type ResearchReport struct {
	Title     string     `json:"title"`
	Summary   string     `json:"summary"`
	Sections  []Section  `json:"sections"`
	Citations []Citation `json:"citations"`
}

// Section is a part of a research report.
type Section struct {
	Heading  string    `json:"heading"`
	Body     string    `json:"body"`
	Findings []Finding `json:"findings,omitempty"`
}

// Client is the OpenSearch RAG client.
type Client struct {
	os  *opensearch.Client
	cfg *config.Config
}

func NewClient(os *opensearch.Client, cfg *config.Config) *Client {
	return &Client{os: os, cfg: cfg}
}

// VectorSearch embeds the query and performs KNN vector search, falling back
// to text match if embedding fails. This is the primary retrieval method.
func (c *Client) VectorSearch(query string, topK int, filters map[string]string) ([]Document, error) {
	if c.os == nil {
		return nil, fmt.Errorf("opensearch client not configured")
	}
	if topK <= 0 {
		topK = 5
	}
	if topK > 20 {
		topK = 20
	}

	ctx := context.Background()
	index := opensearch.IndexEmbeddings
	if c.cfg != nil && c.cfg.RAG != nil && c.cfg.RAG.Index != "" {
		index = c.cfg.RAG.Index
	}

	// Try vector search first
	if c.cfg != nil {
		vector, err := EmbedQuery(ctx, c.cfg, query)
		if err == nil {
			resp, err := c.os.KNNSearch(ctx, index, "vector", vector, topK, filters)
			if err == nil {
				return hitsToDocuments(resp.Hits.Hits), nil
			}
		}
	}

	// Fallback to text search
	return c.textSearch(ctx, query, topK, filters, index)
}

func (c *Client) textSearch(ctx context.Context, query string, topK int, filters map[string]string, index string) ([]Document, error) {
	osQuery := map[string]any{
		"size": topK,
		"query": map[string]any{
			"match": map[string]any{"text": query},
		},
	}

	if len(filters) > 0 {
		var filterClauses []map[string]any
		for k, v := range filters {
			filterClauses = append(filterClauses, map[string]any{
				"term": map[string]any{k: v},
			})
		}
		osQuery["query"] = map[string]any{
			"bool": map[string]any{
				"must":   []any{map[string]any{"match": map[string]any{"text": query}}},
				"filter": filterClauses,
			},
		}
	}

	resp, err := c.os.Search(ctx, index, osQuery)
	if err != nil {
		return nil, fmt.Errorf("opensearch text search: %w", err)
	}
	return hitsToDocuments(resp.Hits.Hits), nil
}

// Search performs a text search against the embeddings index.
func (c *Client) Search(query string, topK int, filters map[string]string) ([]Document, error) {
	if c.os == nil {
		return nil, fmt.Errorf("opensearch client not configured")
	}

	if topK <= 0 {
		topK = 5
	}
	if topK > 10 {
		topK = 10
	}

	ctx := context.Background()
	osQuery := map[string]any{
		"size": topK,
		"query": map[string]any{
			"match": map[string]any{
				"text": query,
			},
		},
	}

	if len(filters) > 0 {
		var filterClauses []map[string]any
		for k, v := range filters {
			filterClauses = append(filterClauses, map[string]any{
				"term": map[string]any{k: v},
			})
		}
		osQuery["query"] = map[string]any{
			"bool": map[string]any{
				"must":   []any{map[string]any{"match": map[string]any{"text": query}}},
				"filter": filterClauses,
			},
		}
	}

	resp, err := c.os.Search(ctx, opensearch.IndexEmbeddings, osQuery)
	if err != nil {
		return nil, fmt.Errorf("opensearch search: %w", err)
	}

	return hitsToDocuments(resp.Hits.Hits), nil
}

// GetByID retrieves a document by its OpenSearch document ID.
func (c *Client) GetByID(id string) (*Document, error) {
	if c.os == nil {
		return nil, fmt.Errorf("opensearch client not configured")
	}

	ctx := context.Background()
	hit, err := c.os.GetDocument(ctx, opensearch.IndexEmbeddings, id)
	if err != nil {
		return nil, fmt.Errorf("opensearch get: %w", err)
	}

	docs := hitsToDocuments([]opensearch.Hit{*hit})
	if len(docs) == 0 {
		return nil, fmt.Errorf("document %s not found", id)
	}
	return &docs[0], nil
}

// Segment is a chunk of a document from the vector store.
type Segment struct {
	SegmentID int    `json:"segment_id"`
	Content   string `json:"content"`
}

// GetSegments returns all segments/chunks for a document by doc_id.
func (c *Client) GetSegments(docID string) ([]Segment, error) {
	if c.os == nil {
		return nil, fmt.Errorf("opensearch client not configured")
	}

	ctx := context.Background()
	query := map[string]any{
		"size": 100,
		"sort": []map[string]any{{"chunk_id": map[string]string{"order": "asc"}}},
		"query": map[string]any{
			"term": map[string]any{"doc_id": docID},
		},
	}

	resp, err := c.os.Search(ctx, opensearch.IndexEmbeddings, query)
	if err != nil {
		return nil, fmt.Errorf("opensearch segments search: %w", err)
	}

	if len(resp.Hits.Hits) == 0 {
		return nil, fmt.Errorf("no segments found for document %s", docID)
	}

	var segments []Segment
	for _, hit := range resp.Hits.Hits {
		var raw map[string]any
		if err := json.Unmarshal(hit.Source, &raw); err != nil {
			continue
		}
		chunkID := 0
		if v, ok := raw["chunk_id"].(float64); ok {
			chunkID = int(v)
		}
		text, _ := raw["text"].(string)
		segments = append(segments, Segment{SegmentID: chunkID, Content: text})
	}
	return segments, nil
}

func hitsToDocuments(hits []opensearch.Hit) []Document {
	docs := make([]Document, 0, len(hits))
	for _, hit := range hits {
		var raw map[string]any
		if err := json.Unmarshal(hit.Source, &raw); err != nil {
			continue
		}
		doc := Document{
			Title:   getString(raw, "title"),
			Content: getString(raw, "text"),
			Metadata: DocumentMetadata{
				DocumentID:      getString(raw, "doc_id"),
				Source:          getString(raw, "source"),
				Author:          getString(raw, "author"),
				Date:            getString(raw, "date"),
				Classification:  getString(raw, "classification"),
				ConfidenceScore: hit.Score,
			},
		}
		if doc.Metadata.DocumentID == "" {
			doc.Metadata.DocumentID = hit.ID
		}
		docs = append(docs, doc)
	}
	return docs
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
