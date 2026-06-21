package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"agentic/internal/config"
)

func Models(cfg *config.Config) http.HandlerFunc {
	// Pre-build the model list at handler creation time
	created := time.Now().Unix()
	var data []map[string]any

	// Add agent models from agents.yaml
	if cfg.Agents != nil {
		for _, a := range cfg.Agents.Agents {
			entry := map[string]any{
				"id":       a.ID,
				"object":   "model",
				"type":     "agent",
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
	if cfg.Models != nil {
		for _, m := range cfg.Models.AllModels() {
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

func Agents(cfg *config.Config) http.HandlerFunc {
	created := time.Now().Unix()
	data := []map[string]any{}

	if cfg.Agents != nil {
		for _, a := range cfg.Agents.Agents {
			if a.Internal {
				continue
			}
			data = append(data, buildAgentEntry(cfg, a, created))
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

func buildAgentEntry(cfg *config.Config, a config.AgentConfig, created int64) map[string]any {
	entry := map[string]any{
		"id":          a.ID,
		"object":      "agent",
		"created":     created,
		"owned_by":    "agentic",
		"type":        a.Type,
		"name":        a.Name,
		"description": a.Description,
		"model":       a.Model,
		"provider":    a.Provider,
		"tools":       a.Tools,
		"sub_agents":  buildSubAgentEntries(cfg, a),
	}
	if a.SystemPrompt != "" {
		entry["system_prompt"] = a.SystemPrompt
	}
	if a.OutputKey != "" {
		entry["output_key"] = a.OutputKey
	}
	if a.OutputAgent != "" {
		entry["output_agent"] = a.OutputAgent
	}
	if len(a.Keywords) > 0 {
		entry["keywords"] = a.Keywords
	}
	if a.MaxIterations > 0 {
		entry["max_iterations"] = a.MaxIterations
	}
	if a.MaxParallelWorkers > 0 {
		entry["max_parallel_workers"] = a.MaxParallelWorkers
	}
	return entry
}

func buildSubAgentEntries(cfg *config.Config, parent config.AgentConfig) []map[string]any {
	if cfg.Agents == nil || len(parent.SubAgents) == 0 {
		return []map[string]any{}
	}
	resolved, err := cfg.Agents.ResolveSubAgents(&parent)
	if err != nil {
		// A single bad id shouldn't fail the whole response; resolve what we can.
		resolved = nil
		for _, id := range parent.SubAgents {
			if sub := cfg.Agents.FindAgent(id); sub != nil {
				resolved = append(resolved, sub)
			}
		}
	}
	entries := make([]map[string]any, 0, len(resolved))
	for _, a := range resolved {
		entry := map[string]any{
			"id":          a.ID,
			"type":        a.Type,
			"name":        a.Name,
			"description": a.Description,
			"model":       a.Model,
			"provider":    a.Provider,
			"tools":       a.Tools,
		}
		if a.SystemPrompt != "" {
			entry["system_prompt"] = a.SystemPrompt
		}
		entries = append(entries, entry)
	}
	return entries
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
		"provider_id":          m.ProviderID,
		"provider_name":        m.ProviderName,
		"supported_parameters": m.EffectiveSupportedParameters(),
		// Capability booleans
		"vision":     m.HasCapability("vision"),
		"tools":      m.HasCapability("tool_calling"),
		"audio":      m.HasCapability("audio"),
		"reasoning":  m.HasCapability("reasoning"),
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
