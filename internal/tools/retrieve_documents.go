package tools

import (
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type RetrieveDocumentsArgs struct {
	Query string `json:"query" desc:"Natural language search query"`
	TopK  int    `json:"top_k" desc:"Number of documents to retrieve (default 3, max 5)"`
}

type RetrieveDocumentsResult struct {
	Query          string `json:"query"`
	TotalRetrieved int    `json:"total_retrieved"`
	Documents      any    `json:"documents"`
}

func NewRetrieveDocumentsTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "retrieve_documents",
		Description: "Search the knowledge base using semantic similarity.",
	}, retrieveDocumentsHandler)
}

func retrieveDocumentsHandler(_ tool.Context, args RetrieveDocumentsArgs) (RetrieveDocumentsResult, error) {
	docs := []map[string]any{
		{
			"id":      "doc_001",
			"title":   "Q4 2024 Business Performance Report",
			"content": "Revenue exceeded targets by 12%. Enterprise segment grew 24% YoY. Cloud Suite became the top-selling product.",
			"score":   0.96,
			"source":  "reports/q4-2024-performance.pdf",
		},
		{
			"id":      "doc_002",
			"title":   "Competitive Landscape Analysis",
			"content": "Market share grew from 19.1% to 23.4%. Three main competitors: Acme Corp (31%), TechCo (18%), NovaSoft (12%).",
			"score":   0.89,
			"source":  "research/competitive-analysis-2024.pdf",
		},
		{
			"id":      "doc_003",
			"title":   "Product Roadmap 2025",
			"content": "Q1: AI assistant integration. Q2: API-first redesign. Q3: Mobile apps. Q4: Enterprise SSO.",
			"score":   0.83,
			"source":  "product/roadmap-2025.md",
		},
	}

	topK := args.TopK
	if topK <= 0 {
		topK = 3
	}
	if topK > 5 {
		topK = 5
	}
	if topK > len(docs) {
		topK = len(docs)
	}

	return RetrieveDocumentsResult{
		Query:          args.Query,
		TotalRetrieved: topK,
		Documents:      docs[:topK],
	}, nil
}
