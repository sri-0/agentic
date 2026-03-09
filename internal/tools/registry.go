package tools

import (
	"fmt"

	"agentic/internal/rag"

	"google.golang.org/adk/tool"
)

// NewAllTools creates all available tools for the agent (backward compat for test-agent).
func NewAllTools() ([]tool.Tool, error) {
	qdb, err := NewQueryDatabaseTool()
	if err != nil {
		return nil, err
	}
	wdb, err := NewWriteDatabaseTool()
	if err != nil {
		return nil, err
	}
	rd, err := NewRetrieveDocumentsTool()
	if err != nil {
		return nil, err
	}
	ws, err := NewWebSearchTool()
	if err != nil {
		return nil, err
	}
	calc, err := NewCalculateTool()
	if err != nil {
		return nil, err
	}
	return []tool.Tool{qdb, wdb, rd, ws, calc}, nil
}

// ToolNames returns the names of all tools.
func ToolNames() []string {
	return []string{"query_database", "write_database", "retrieve_documents", "web_search", "calculate"}
}

// HITLToolNames returns tool names that require human approval.
func HITLToolNames() []string {
	return []string{"write_database"}
}

// NewToolByName creates a tool by name. The ragClient is used for tools that need RAG access.
// Pass nil for ragClient if the tool doesn't need it.
func NewToolByName(name string, ragClient *rag.Client) (tool.Tool, error) {
	switch name {
	// Original tools
	case "query_database":
		return NewQueryDatabaseTool()
	case "write_database":
		return NewWriteDatabaseTool()
	case "retrieve_documents":
		return NewRetrieveDocumentsTool()
	case "web_search":
		return NewWebSearchTool()
	case "calculate":
		return NewCalculateTool()

	// RAG tools
	case "opensearch_retrieve":
		return NewOpenSearchRetrieveTool(ragClient)
	case "opensearch_retrieve_by_id":
		return NewOpenSearchRetrieveByIDTool(ragClient)

	// Analyst tools
	case "summarize_documents":
		return NewSummarizeDocumentsTool()
	case "extract_findings":
		return NewExtractFindingsTool()

	// Database research tools
	case "query_research_db":
		return NewQueryResearchDBTool()
	case "query_metrics_db":
		return NewQueryMetricsDBTool()

	// Report tool
	case "generate_report":
		return NewGenerateReportTool()

	// Alert tool
	case "trigger_alert":
		return NewTriggerAlertTool()

	// Incident tools
	case "classify_incident":
		return NewClassifyIncidentTool()
	case "get_incident_context":
		return NewGetIncidentContextTool()

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
