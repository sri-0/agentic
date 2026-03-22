package tools

import (
	"context"

	"agentic/internal/tools/confluence"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type ConfluenceSearchArgs struct {
	Query string `json:"query" desc:"CQL search query (e.g. type=page AND text~\"kubernetes\"). For simple text search, use: text~\"your search terms\""`
	Limit int    `json:"limit" desc:"Maximum number of results to return (default 10, max 50)"`
}

type ConfluenceSearchResult struct {
	Query   string                      `json:"query"`
	Total   int                         `json:"total"`
	Results []confluence.SearchResult    `json:"results"`
}

func NewConfluenceSearchTool(client *confluence.Client) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "confluence_search",
		Description: "Search Confluence for pages and content using CQL (Confluence Query Language). Returns page titles, excerpts, and IDs that can be read with confluence_read_page.",
	}, func(ctx tool.Context, args ConfluenceSearchArgs) (ConfluenceSearchResult, error) {
		if client == nil {
			return ConfluenceSearchResult{Query: args.Query}, nil
		}

		resp, err := client.Search(context.Background(), args.Query, args.Limit)
		if err != nil {
			return ConfluenceSearchResult{Query: args.Query}, err
		}

		results := resp.Results
		if results == nil {
			results = []confluence.SearchResult{}
		}

		return ConfluenceSearchResult{
			Query:   args.Query,
			Total:   resp.TotalSize,
			Results: results,
		}, nil
	})
}
