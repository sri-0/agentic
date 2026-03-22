package tools

import (
	"context"

	"agentic/internal/tools/confluence"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type ConfluenceReadPageArgs struct {
	PageID string `json:"page_id" desc:"Confluence page ID to retrieve (from confluence_search results)"`
}

type ConfluenceReadPageResult struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	SpaceKey  string   `json:"space_key"`
	Body      string   `json:"body"`
	Version   int      `json:"version"`
	Ancestors []string `json:"ancestors"`
	URL       string   `json:"url"`
}

func NewConfluenceReadPageTool(client *confluence.Client) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "confluence_read_page",
		Description: "Read the full content of a Confluence page by its ID. Returns the page body as plain text, along with metadata like space, version, and parent page hierarchy.",
	}, func(ctx tool.Context, args ConfluenceReadPageArgs) (ConfluenceReadPageResult, error) {
		if client == nil {
			return ConfluenceReadPageResult{ID: args.PageID}, nil
		}

		page, err := client.GetPage(context.Background(), args.PageID)
		if err != nil {
			return ConfluenceReadPageResult{ID: args.PageID}, err
		}

		ancestors := page.Ancestors
		if ancestors == nil {
			ancestors = []string{}
		}

		return ConfluenceReadPageResult{
			ID:        page.ID,
			Title:     page.Title,
			SpaceKey:  page.SpaceKey,
			Body:      confluence.StripHTML(page.BodyHTML),
			Version:   page.Version,
			Ancestors: ancestors,
			URL:       page.URL,
		}, nil
	})
}
