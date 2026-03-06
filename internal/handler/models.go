package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agentic/internal/agent"
)

func Models(core *agent.Core) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{
					"id":          core.Config.AgentModelName,
					"object":      "model",
					"created":     time.Now().Unix(),
					"owned_by":    "local",
					"description": fmt.Sprintf("ADK ReAct agent via %s", core.Config.LLMModel),
				},
			},
		})
	}
}
