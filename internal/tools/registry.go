package tools

import (
	"fmt"

	"agentic/internal/rag"
	"agentic/internal/tools/confluence"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
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
		"confluence_search", "confluence_read_page",
		"view_skill",
		"emit_artifact",
		"todowrite",
		"task",
		"task_join",
		"question",
		"search_memories", "add_memory", "update_memory", "delete_memory", "list_memories",
	}
}

// HITLToolNames returns tool names that pause the run for human input (approval
// or, for the question tool, an answer).
func HITLToolNames() []string {
	return []string{"write_database", "question"}
}

// Deps holds shared dependencies for tool construction.
type Deps struct {
	RAGClient        *rag.Client
	OSClient         *opensearch.Client
	ConfluenceClient *confluence.Client
	MemoryTools      map[string]tool.Tool // keyed by tool name
	TaskTool         tool.Tool            // fallback swarm dispatch tool (no per-agent governance)
	TaskJoinTool     tool.Tool            // fallback join tool paired with TaskTool
	// TaskFactory builds a governed (task, task_join) pair for a specific
	// coordinator, restricted to its AllowedSubagents. When set it is preferred
	// over the shared TaskTool/TaskJoinTool so each coordinator only sees the
	// subagents it is allowed to dispatch.
	TaskFactory func(allowed []string) (task tool.Tool, join tool.Tool, err error)
	MCPToolsets      func(servers []string) []tool.Toolset // resolves MCP server names to ADK toolsets
	Logger           zerolog.Logger
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
	case "confluence_search":
		return NewConfluenceSearchTool(deps.ConfluenceClient)
	case "confluence_read_page":
		return NewConfluenceReadPageTool(deps.ConfluenceClient)
	case "view_skill":
		return NewViewSkillTool(deps.OSClient)
	case "emit_artifact":
		return NewEmitArtifactTool()
	case "todowrite":
		return NewTodoWriteTool()
	case "question":
		return NewQuestionTool()
	case "task":
		if deps.TaskTool != nil {
			return deps.TaskTool, nil
		}
		return nil, fmt.Errorf("task tool not available (no roster/session configured)")
	case "task_join":
		if deps.TaskJoinTool != nil {
			return deps.TaskJoinTool, nil
		}
		return nil, fmt.Errorf("task_join tool not available (no roster/session configured)")
	case "search_memories", "add_memory", "update_memory", "delete_memory", "list_memories":
		if t, ok := deps.MemoryTools[name]; ok {
			return t, nil
		}
		return nil, fmt.Errorf("memory tool %s not available (no memory service configured)", name)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
