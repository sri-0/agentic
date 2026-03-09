package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"agentic/internal/agent"
	"agentic/internal/config"
)

func Models(core *agent.Core) http.HandlerFunc {
	// Pre-build the model list at handler creation time
	created := time.Now().Unix()
	var data []map[string]any

	// Add agent models from agents.yaml
	if core.Config.Agents != nil {
		for _, a := range core.Config.Agents.Agents {
			entry := map[string]any{
				"id":       a.ID,
				"object":   "model",
				"created":  created,
				"owned_by": "agentic",
			}
			if a.Description != "" {
				entry["description"] = a.Description
			}
			data = append(data, entry)
		}
	}

	// Add upstream models from models.yaml
	if core.Config.Models != nil {
		for _, m := range core.Config.Models.AllModels() {
			data = append(data, buildModelEntry(m, created))
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	}
}

func buildModelEntry(m config.Model, fallbackCreated int64) map[string]any {
	created := m.CreatedUnix()
	if created == 0 {
		created = fallbackCreated
	}

	entry := map[string]any{
		"id":                   m.ID,
		"object":               "model",
		"type":                 string(m.Type),
		"created":              created,
		"owned_by":             m.OwnedBy,
		"supported_parameters": m.EffectiveSupportedParameters(),
		// Capability booleans
		"vision":    m.HasCapability("vision"),
		"tools":     m.HasCapability("tool_calling"),
		"audio":     m.HasCapability("audio"),
		"reasoning": m.HasCapability("reasoning"),
		"multimodal": m.IsMultimodal(),
	}
	if m.Name != "" {
		entry["name"] = m.Name
	}
	if m.Description != "" {
		entry["description"] = m.Description
	}
	if m.ContextLength > 0 {
		entry["context_length"] = m.ContextLength
	}
	if len(m.Capabilities) > 0 {
		entry["capabilities"] = m.Capabilities
	}
	if m.Architecture != nil {
		arch := map[string]any{
			"input_modalities":  m.Architecture.InputModalities,
			"output_modalities": m.Architecture.OutputModalities,
			"modality":          m.Architecture.Modality(),
		}
		if m.Architecture.Tokenizer != "" {
			arch["tokenizer"] = m.Architecture.Tokenizer
		}
		if m.Architecture.InstructType != "" {
			arch["instruct_type"] = m.Architecture.InstructType
		}
		entry["architecture"] = arch
	}
	return entry
}
