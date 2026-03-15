package tools

import (
	"agentic/internal/rag"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type RetrieveDocumentsArgs struct {
	Query   string            `json:"query" desc:"Natural language search query for semantic retrieval"`
	TopK    int               `json:"top_k" desc:"Number of documents to retrieve (default 5, max 20)"`
	Filters map[string]string `json:"filters,omitempty" desc:"Optional metadata filters (e.g. classification, author, source)"`
}

type RetrieveDocumentsResult struct {
	Query     string         `json:"query"`
	Total     int            `json:"total"`
	Documents []rag.Document `json:"documents"`
}

func NewRetrieveDocumentsTool(client *rag.Client) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "retrieve_documents",
		Description: "Search the knowledge base using semantic similarity. Embeds the query and performs vector search against the document store, returning the most relevant documents with metadata for citation.",
	}, func(_ tool.Context, args RetrieveDocumentsArgs) (RetrieveDocumentsResult, error) {
		topK := args.TopK
		if topK <= 0 {
			topK = 5
		}

		docs, err := client.VectorSearch(args.Query, topK, args.Filters)
		if err != nil {
			return RetrieveDocumentsResult{Query: args.Query}, err
		}

		return RetrieveDocumentsResult{
			Query:     args.Query,
			Total:     len(docs),
			Documents: docs,
		}, nil
	})
}
