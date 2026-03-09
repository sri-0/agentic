package rag

import (
	"fmt"
	"strings"
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
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Sections  []Section `json:"sections"`
	Citations []Citation `json:"citations"`
}

// Section is a part of a research report.
type Section struct {
	Heading  string    `json:"heading"`
	Body     string    `json:"body"`
	Findings []Finding `json:"findings,omitempty"`
}

// Client is the OpenSearch RAG client. Currently returns mock data.
type Client struct {
	// Will hold OpenSearch connection config
}

func NewClient() *Client { return &Client{} }

// allDocs is the shared mock document corpus.
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

// Search performs a semantic search (mock). Returns up to topK documents matching optional filters.
func (c *Client) Search(query string, topK int, filters map[string]string) ([]Document, error) {
	if topK <= 0 {
		topK = 5
	}
	if topK > 10 {
		topK = 10
	}

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

// GetByID retrieves a document by its ID (mock).
func (c *Client) GetByID(id string) (*Document, error) {
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
