package tools

import (
	"google.golang.org/adk/tool"
)

// NewAllTools creates all available tools for the agent.
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
