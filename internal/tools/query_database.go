package tools

import (
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type QueryDatabaseArgs struct {
	SQL string `json:"sql" desc:"SQL-like query string describing what data to fetch"`
}

type QueryDatabaseResult struct {
	Query    string `json:"query"`
	Table    string `json:"table"`
	RowCount int    `json:"row_count"`
	Rows     any    `json:"rows"`
}

func NewQueryDatabaseTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "query_database",
		Description: "Query the internal analytics database. Read-only; returns rows matching the query.",
	}, queryDatabaseHandler)
}

func queryDatabaseHandler(_ tool.Context, args QueryDatabaseArgs) (QueryDatabaseResult, error) {
	products := []map[string]any{
		{"id": 1, "name": "Widget Pro", "revenue": 42000, "units": 840, "quarter": "Q4 2024"},
		{"id": 2, "name": "Gadget Max", "revenue": 31500, "units": 630, "quarter": "Q4 2024"},
		{"id": 3, "name": "Device Lite", "revenue": 18750, "units": 1250, "quarter": "Q4 2024"},
		{"id": 4, "name": "Cloud Suite", "revenue": 95000, "units": 190, "quarter": "Q4 2024"},
		{"id": 5, "name": "Analytics+", "revenue": 67200, "units": 448, "quarter": "Q4 2024"},
	}
	orders := []map[string]any{
		{"order_id": 1001, "customer": "Alice Chen", "amount": 2400, "date": "2024-10-15"},
		{"order_id": 1002, "customer": "Bob Martinez", "amount": 149, "date": "2024-10-22"},
		{"order_id": 1003, "customer": "Carol Smith", "amount": 1800, "date": "2024-11-03"},
		{"order_id": 1004, "customer": "David Lee", "amount": 320, "date": "2024-11-18"},
		{"order_id": 1005, "customer": "Eva Patel", "amount": 4500, "date": "2024-12-01"},
		{"order_id": 1006, "customer": "Frank Wu", "amount": 980, "date": "2024-12-08"},
		{"order_id": 1007, "customer": "Grace Kim", "amount": 2150, "date": "2024-12-14"},
		{"order_id": 1008, "customer": "Henry James", "amount": 720, "date": "2024-12-20"},
	}
	users := []map[string]any{
		{"id": 1, "name": "Alice Chen", "plan": "enterprise", "mrr": 2400, "active": true},
		{"id": 2, "name": "Bob Martinez", "plan": "pro", "mrr": 149, "active": false},
		{"id": 3, "name": "Carol Smith", "plan": "enterprise", "mrr": 1800, "active": true},
		{"id": 4, "name": "David Lee", "plan": "free", "mrr": 0, "active": false},
		{"id": 5, "name": "Eva Patel", "plan": "pro", "mrr": 149, "active": false},
	}
	metrics := []map[string]any{
		{"metric": "total_mrr", "value": 284000, "change_pct": 12.3},
		{"metric": "churn_rate", "value": 2.1, "change_pct": -0.4},
		{"metric": "nps_score", "value": 67, "change_pct": 3.2},
		{"metric": "active_users", "value": 14820, "change_pct": 8.7},
	}

	lower := strings.ToLower(args.SQL)
	switch {
	case strings.Contains(lower, "order"):
		return QueryDatabaseResult{Query: args.SQL, Table: "orders", RowCount: 8, Rows: orders}, nil
	case strings.Contains(lower, "user"):
		return QueryDatabaseResult{Query: args.SQL, Table: "users", RowCount: 5, Rows: users}, nil
	case strings.Contains(lower, "metric") || strings.Contains(lower, "kpi") || strings.Contains(lower, "mrr"):
		return QueryDatabaseResult{Query: args.SQL, Table: "metrics", RowCount: 4, Rows: metrics}, nil
	default:
		return QueryDatabaseResult{Query: args.SQL, Table: "products", RowCount: 5, Rows: products}, nil
	}
}
