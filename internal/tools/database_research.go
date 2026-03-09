package tools

import (
	"strings"

	"agentic/internal/rag"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ── query_research_db ──────────────────────────────────────────────────────

type QueryResearchDBArgs struct {
	SQL string `json:"sql" desc:"SQL query for the research database (read-only)"`
}

func NewQueryResearchDBTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "query_research_db",
		Description: "Query the research database for market data, financials, and historical metrics. Read-only.",
	}, queryResearchDBHandler)
}

func queryResearchDBHandler(_ tool.Context, args QueryResearchDBArgs) (rag.DatabaseResult, error) {
	lower := strings.ToLower(args.SQL)

	switch {
	case strings.Contains(lower, "revenue") || strings.Contains(lower, "financial"):
		return rag.DatabaseResult{
			Query:    args.SQL,
			Table:    "financials",
			RowCount: 4,
			Rows: []map[string]any{
				{"quarter": "Q1 2024", "revenue": 210000, "growth_pct": 8.2, "margin_pct": 14.1},
				{"quarter": "Q2 2024", "revenue": 228000, "growth_pct": 8.6, "margin_pct": 15.3},
				{"quarter": "Q3 2024", "revenue": 245000, "growth_pct": 7.5, "margin_pct": 15.1},
				{"quarter": "Q4 2024", "revenue": 284000, "growth_pct": 15.9, "margin_pct": 18.3},
			},
		}, nil

	case strings.Contains(lower, "market") || strings.Contains(lower, "competitor"):
		return rag.DatabaseResult{
			Query:    args.SQL,
			Table:    "market_share",
			RowCount: 4,
			Rows: []map[string]any{
				{"company": "Acme Corp", "share_pct": 31.0, "yoy_change": -1.2},
				{"company": "Us", "share_pct": 23.4, "yoy_change": 4.3},
				{"company": "TechCo", "share_pct": 18.0, "yoy_change": -0.8},
				{"company": "NovaSoft", "share_pct": 12.0, "yoy_change": -0.5},
			},
		}, nil

	case strings.Contains(lower, "customer") || strings.Contains(lower, "churn"):
		return rag.DatabaseResult{
			Query:    args.SQL,
			Table:    "customer_metrics",
			RowCount: 4,
			Rows: []map[string]any{
				{"quarter": "Q1 2024", "active_users": 11200, "churn_rate": 3.1, "nps": 58},
				{"quarter": "Q2 2024", "active_users": 12400, "churn_rate": 2.8, "nps": 61},
				{"quarter": "Q3 2024", "active_users": 13600, "churn_rate": 2.4, "nps": 64},
				{"quarter": "Q4 2024", "active_users": 14820, "churn_rate": 2.1, "nps": 67},
			},
		}, nil

	default:
		return rag.DatabaseResult{
			Query:    args.SQL,
			Table:    "general",
			RowCount: 3,
			Rows: []map[string]any{
				{"metric": "total_mrr", "value": 284000, "change_pct": 12.3},
				{"metric": "active_users", "value": 14820, "change_pct": 8.7},
				{"metric": "avg_deal_size", "value": 4200, "change_pct": 15.1},
			},
		}, nil
	}
}

// ── query_metrics_db ───────────────────────────────────────────────────────

type QueryMetricsDBArgs struct {
	MetricNames []string `json:"metric_names" desc:"Names of metrics to query (e.g. revenue, churn_rate, nps)"`
	DateRange   string   `json:"date_range" desc:"Date range (e.g. '2024-01-01 to 2024-12-31')"`
}

func NewQueryMetricsDBTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "query_metrics_db",
		Description: "Query time-series metrics from the metrics database. Returns metric values over the specified date range.",
	}, queryMetricsDBHandler)
}

func queryMetricsDBHandler(_ tool.Context, args QueryMetricsDBArgs) (rag.DatabaseResult, error) {
	var rows []map[string]any
	for _, name := range args.MetricNames {
		rows = append(rows, []map[string]any{
			{"metric": name, "period": "2024-Q1", "value": 100},
			{"metric": name, "period": "2024-Q2", "value": 112},
			{"metric": name, "period": "2024-Q3", "value": 119},
			{"metric": name, "period": "2024-Q4", "value": 135},
		}...)
	}

	return rag.DatabaseResult{
		Query:    "metrics: " + strings.Join(args.MetricNames, ", "),
		Table:    "time_series_metrics",
		Rows:     rows,
		RowCount: len(rows),
	}, nil
}
