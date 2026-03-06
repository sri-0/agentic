package handler

import (
	"encoding/json"
	"net/http"

	"agentic/internal/agent"
	"agentic/internal/tools"
)

func Health(core *agent.Core) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"model":      core.Config.LLMModel,
			"tools":      tools.ToolNames(),
			"hitl_tools": tools.HITLToolNames(),
		})
	}
}
