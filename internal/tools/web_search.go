package tools

import (
	"fmt"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type WebSearchArgs struct {
	Query      string `json:"query" desc:"Search query string"`
	NumResults int    `json:"num_results" desc:"Number of results to return (default 4)"`
}

type WebSearchResult struct {
	Query   string `json:"query"`
	Results any    `json:"results"`
}

func NewWebSearchTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "web_search",
		Description: "Search the web for current information and news.",
	}, webSearchHandler)
}

func webSearchHandler(_ tool.Context, args WebSearchArgs) (WebSearchResult, error) {
	results := []map[string]any{
		{
			"url":       "https://techcrunch.com/2025/02/ai-market-growth",
			"title":     "AI Market Expected to Reach $1.8T by 2030",
			"snippet":   "Analysts project 38% CAGR. Enterprise adoption is the primary driver.",
			"published": "2025-02-18",
		},
		{
			"url":       "https://gartner.com/insights/2025-tech-predictions",
			"title":     "Gartner's Top 10 Tech Trends for 2025",
			"snippet":   "AI agents and autonomous systems top the list.",
			"published": "2025-01-15",
		},
		{
			"url":       "https://bloomberg.com/news/saas-consolidation-2025",
			"title":     "SaaS Consolidation Wave Accelerates",
			"snippet":   fmt.Sprintf("Related to '%s': Major SaaS players acquiring AI startups. M&A activity up 67%%.", args.Query),
			"published": "2025-02-10",
		},
	}

	numResults := args.NumResults
	if numResults <= 0 {
		numResults = 4
	}
	if numResults > len(results) {
		numResults = len(results)
	}

	return WebSearchResult{
		Query:   args.Query,
		Results: results[:numResults],
	}, nil
}
