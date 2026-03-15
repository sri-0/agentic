package tools

import (
	"context"
	"encoding/json"

	"agentic/pkg/db/opensearch"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type QueryDatabaseArgs struct {
	Query string `json:"query" desc:"Natural language query describing what data to find"`
	Index string `json:"index" desc:"OpenSearch index to query (e.g. products, orders, metrics). Leave empty to search all data indices."`
	Size  int    `json:"size" desc:"Maximum number of results to return (default 10, max 100)"`
}

type QueryDatabaseResult struct {
	Query    string           `json:"query"`
	Index    string           `json:"index"`
	RowCount int              `json:"row_count"`
	Rows     []map[string]any `json:"rows"`
}

func NewQueryDatabaseTool(osClient *opensearch.Client) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "query_database",
		Description: "Query the database for structured data. Performs a text search across the specified index and returns matching records.",
	}, func(_ tool.Context, args QueryDatabaseArgs) (QueryDatabaseResult, error) {
		if osClient == nil {
			return QueryDatabaseResult{Query: args.Query}, nil
		}

		size := args.Size
		if size <= 0 {
			size = 10
		}
		if size > 100 {
			size = 100
		}

		index := args.Index
		if index == "" {
			index = "_all"
		}

		osQuery := map[string]any{
			"size": size,
			"query": map[string]any{
				"multi_match": map[string]any{
					"query": args.Query,
					"type":  "best_fields",
				},
			},
		}

		resp, err := osClient.Search(context.Background(), index, osQuery)
		if err != nil {
			return QueryDatabaseResult{Query: args.Query, Index: index}, err
		}

		rows := make([]map[string]any, 0, len(resp.Hits.Hits))
		for _, hit := range resp.Hits.Hits {
			var row map[string]any
			if err := json.Unmarshal(hit.Source, &row); err != nil {
				continue
			}
			row["_id"] = hit.ID
			row["_score"] = hit.Score
			rows = append(rows, row)
		}

		return QueryDatabaseResult{
			Query:    args.Query,
			Index:    index,
			RowCount: len(rows),
			Rows:     rows,
		}, nil
	})
}
