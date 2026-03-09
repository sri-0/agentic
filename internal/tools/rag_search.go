package tools

import (
	"agentic/internal/rag"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ── opensearch_retrieve ────────────────────────────────────────────────────

type OpenSearchRetrieveArgs struct {
	Query   string            `json:"query" desc:"Natural language search query for semantic retrieval"`
	TopK    int               `json:"top_k" desc:"Number of documents to retrieve (default 5, max 10)"`
	Filters map[string]string `json:"filters,omitempty" desc:"Optional metadata filters (e.g. classification, author)"`
}

type OpenSearchRetrieveResult struct {
	Query     string         `json:"query"`
	Total     int            `json:"total"`
	Documents []rag.Document `json:"documents"`
}

func NewOpenSearchRetrieveTool(client *rag.Client) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "opensearch_retrieve",
		Description: "Semantic search against the OpenSearch vector database. Returns documents with full metadata for citation tracking.",
	}, func(_ tool.Context, args OpenSearchRetrieveArgs) (OpenSearchRetrieveResult, error) {
		docs, err := client.Search(args.Query, args.TopK, args.Filters)
		if err != nil {
			return OpenSearchRetrieveResult{Query: args.Query}, err
		}
		return OpenSearchRetrieveResult{
			Query:     args.Query,
			Total:     len(docs),
			Documents: docs,
		}, nil
	})
}

