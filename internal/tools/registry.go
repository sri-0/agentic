package tools

import (
	"fmt"

	"agentic/internal/rag"
	"agentic/pkg/db/opensearch"

	"google.golang.org/adk/tool"
)

// ToolNames returns all registered tool names.
func ToolNames() []string {
	return []string{
		"query_database", "write_database", "retrieve_documents", "web_search", "calculate",
		"opensearch_retrieve",
		"query_research_db", "query_metrics_db",
		"trigger_alert",
		"classify_incident", "get_incident_context",
	}
}

// HITLToolNames returns tool names that require human approval.
func HITLToolNames() []string {
	return []string{"write_database"}
}

// Deps holds shared dependencies for tool construction.
type Deps struct {
	RAGClient *rag.Client
	OSClient  *opensearch.Client
}

// NewToolByName creates a tool by name.
func NewToolByName(name string, deps Deps) (tool.Tool, error) {
	switch name {
	case "query_database":
		return NewQueryDatabaseTool(deps.OSClient)
	case "write_database":
		return NewWriteDatabaseTool()
	case "retrieve_documents":
		return NewRetrieveDocumentsTool(deps.RAGClient)
	case "web_search":
		return NewWebSearchTool()
	case "calculate":
		return NewCalculateTool()
	case "opensearch_retrieve":
		return NewOpenSearchRetrieveTool(deps.RAGClient)
	case "query_research_db":
		return NewQueryResearchDBTool()
	case "query_metrics_db":
		return NewQueryMetricsDBTool()
	case "trigger_alert":
		return NewTriggerAlertTool()
	case "classify_incident":
		return NewClassifyIncidentTool()
	case "get_incident_context":
		return NewGetIncidentContextTool()
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
