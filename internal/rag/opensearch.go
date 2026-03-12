package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	Title    string    `json:"title"`
	Summary  string    `json:"summary"`
	Sections []Section `json:"sections"`
	Citations []Citation `json:"citations"`
}

// Section is a part of a research report.
type Section struct {
	Heading  string    `json:"heading"`
	Body     string    `json:"body"`
	Findings []Finding `json:"findings,omitempty"`
}

// Client is the OpenSearch RAG client. Falls back to mock data if OpenSearch is nil.
type Client struct {
	os *opensearch.Client
}

func NewClient(os *opensearch.Client) *Client { return &Client{os: os} }

// Search performs a semantic search. Uses OpenSearch kNN if available, else mock.
func (c *Client) Search(query string, topK int, filters map[string]string) ([]Document, error) {
	if topK <= 0 {
		topK = 5
	}
	if topK > 10 {
		topK = 10
	}

	// Try OpenSearch first
	if c.os != nil {
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
		if err == nil {
			return hitsToDocuments(resp.Hits.Hits), nil
		}
		// Fall through to mock on error
	}

	return c.mockSearch(query, topK, filters)
}

// GetByID retrieves a document by its ID.
func (c *Client) GetByID(id string) (*Document, error) {
	if c.os != nil {
		ctx := context.Background()
		hit, err := c.os.GetDocument(ctx, opensearch.IndexEmbeddings, id)
		if err == nil {
			docs := hitsToDocuments([]opensearch.Hit{*hit})
			if len(docs) > 0 {
				return &docs[0], nil
			}
		}
	}

	// Fall back to mock
	for _, doc := range allDocs {
		if doc.Metadata.DocumentID == id {
			return &doc, nil
		}
	}
	return nil, fmt.Errorf("document %s not found", id)
}

// Segment is a chunk of a document from the vector store.
type Segment struct {
	SegmentID int    `json:"segment_id"`
	Content   string `json:"content"`
}

// GetSegments returns all segments/chunks for a document by ID.
func (c *Client) GetSegments(docID string) ([]Segment, error) {
	// Try OpenSearch: search for all chunks with this doc_id
	if c.os != nil {
		ctx := context.Background()
		query := map[string]any{
			"size": 100,
			"sort": []map[string]any{{"chunk_id": map[string]string{"order": "asc"}}},
			"query": map[string]any{
				"term": map[string]any{"doc_id": docID},
			},
		}
		resp, err := c.os.Search(ctx, opensearch.IndexEmbeddings, query)
		if err == nil && len(resp.Hits.Hits) > 0 {
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
	}

	// Fall back to mock
	doc, err := c.GetByID(docID)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitAfter(doc.Content, ".")
	var segments []Segment
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			segments = append(segments, Segment{SegmentID: i + 1, Content: p})
		}
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

// ── Mock data (fallback when OpenSearch is unavailable) ─────────────────────

var allDocs = []Document{
	{
		Metadata: DocumentMetadata{
			DocumentID:      "doc_001",
			Source:          "reports/q4-2024-performance.pdf",
			Author:          "Finance Team",
			Date:            "2025-01-15",
			Classification:  "internal",
			ConfidenceScore: 0.96,
		},
		Title:   "Q4 2024 Business Performance Report",
		Content: "Revenue exceeded targets by 12%. Enterprise segment grew 24% YoY. Cloud Suite became the top-selling product with $95K in revenue. Total MRR reached $284K with 14,820 active users.",
	},
	{
		Metadata: DocumentMetadata{
			DocumentID:      "doc_002",
			Source:          "research/competitive-analysis-2024.pdf",
			Author:          "Strategy Team",
			Date:            "2024-12-20",
			Classification:  "confidential",
			ConfidenceScore: 0.91,
		},
		Title:   "Competitive Landscape Analysis 2024",
		Content: "Market share grew from 19.1% to 23.4%. Three main competitors: Acme Corp (31%), TechCo (18%), NovaSoft (12%). Key differentiator is our AI-first approach and enterprise integrations.",
	},
	{
		Metadata: DocumentMetadata{
			DocumentID:      "doc_003",
			Source:          "product/roadmap-2025.md",
			Author:          "Product Team",
			Date:            "2025-01-10",
			Classification:  "internal",
			ConfidenceScore: 0.87,
		},
		Title:   "Product Roadmap 2025",
		Content: "Q1: AI assistant integration. Q2: API-first redesign with GraphQL. Q3: Mobile apps for iOS and Android. Q4: Enterprise SSO and SCIM provisioning.",
	},
	{
		Metadata: DocumentMetadata{
			DocumentID:      "doc_004",
			Source:          "reports/customer-satisfaction-q4.pdf",
			Author:          "Customer Success",
			Date:            "2025-01-08",
			Classification:  "internal",
			ConfidenceScore: 0.82,
		},
		Title:   "Customer Satisfaction Report Q4 2024",
		Content: "NPS score improved from 64 to 67. Enterprise customers report 94% satisfaction rate. Top requests: better API documentation, mobile access, and SSO support. Churn rate decreased to 2.1%.",
	},
	{
		Metadata: DocumentMetadata{
			DocumentID:      "doc_005",
			Source:          "research/ai-market-trends-2025.pdf",
			Author:          "Research Team",
			Date:            "2025-02-01",
			Classification:  "public",
			ConfidenceScore: 0.78,
		},
		Title:   "AI Market Trends 2025",
		Content: "The enterprise AI market is projected to reach $150B by 2027. Key trends: agentic AI workflows, RAG-based knowledge systems, and AI-native developer tools. Companies investing in AI see 3x productivity gains.",
	},
}

func (c *Client) mockSearch(query string, topK int, filters map[string]string) ([]Document, error) {
	var filtered []Document
	for _, doc := range allDocs {
		if matchesFilters(doc, filters) {
			filtered = append(filtered, doc)
		}
	}
	if topK > len(filtered) {
		topK = len(filtered)
	}
	return filtered[:topK], nil
}

func matchesFilters(doc Document, filters map[string]string) bool {
	for k, v := range filters {
		switch k {
		case "classification":
			if doc.Metadata.Classification != v {
				return false
			}
		case "author":
			if doc.Metadata.Author != v {
				return false
			}
		}
	}
	return true
}
