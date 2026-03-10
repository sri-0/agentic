package deepresearch

import "google.golang.org/adk/agent"

// State keys used for inter-agent communication via session state.
const (
	KeyResearchPlan    = "research_plan"
	KeyDocIDs          = "doc_ids"
	KeyDocCount        = "doc_count"
	KeyDocIndex        = "doc_index"
	KeyCurrentDocument = "current_document"
	KeyAnalysisFindings = "analysis_findings"
	KeyDatabaseResults  = "database_results"
	KeyCurrentFindings  = "current_findings"
	KeyAllFindings      = "all_findings"
	KeyResearchGaps     = "research_gaps"
	KeyDraftReport      = "draft_report"
	KeyCriticFeedback   = "critic_feedback"
)

func stateGet(ctx agent.InvocationContext, key string) any {
	v, _ := ctx.Session().State().Get(key)
	return v
}

func stateInt(ctx agent.InvocationContext, key string) int {
	switch v := stateGet(ctx, key).(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

func stateString(ctx agent.InvocationContext, key string) string {
	v, _ := stateGet(ctx, key).(string)
	return v
}
