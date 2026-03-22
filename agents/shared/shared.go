package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"agentic/internal/config"
	"agentic/internal/tools"
	"agentic/pkg/db/opensearch"
	genaiopenai "agentic/pkg/genai/openai"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
)

// ResolveProvider returns the base URL, API key, and optional custom HTTP client
// for the given agent's provider. The HTTP client is non-nil when the provider
// requires mTLS or a custom CA bundle.
func ResolveProvider(cfg *config.Config, agentCfg *config.AgentConfig) (string, string, *http.Client) {
	if cfg.Models != nil && agentCfg.Provider != "" {
		if p := cfg.Models.FindProvider(agentCfg.Provider); p != nil {
			httpClient, _ := p.HTTPClient()
			return p.BaseURL, p.APIKey(), httpClient
		}
	}
	return "", "", nil
}

func ResolveTools(names []string, deps tools.Deps) ([]tool.Tool, error) {
	var resolved []tool.Tool
	for _, name := range names {
		t, err := tools.NewToolByName(name, deps)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", name, err)
		}
		resolved = append(resolved, t)
	}
	return resolved, nil
}

func BuildLLMAgent(cfg *config.Config, agentCfg *config.AgentConfig, deps tools.Deps) (agent.Agent, error) {
	baseURL, apiKey, httpClient := ResolveProvider(cfg, agentCfg)
	if baseURL == "" {
		return nil, fmt.Errorf("no provider for model %s", agentCfg.Model)
	}

	m := genaiopenai.New(genaiopenai.Config{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		ModelName:  agentCfg.Model,
		HTTPClient: httpClient,
	})

	agentTools, err := ResolveTools(agentCfg.Tools, deps)
	if err != nil {
		return nil, err
	}

	llmCfg := llmagent.Config{
		Name:                    agentCfg.Name,
		Description:             agentCfg.Description,
		Model:                   m,
		Instruction:             agentCfg.SystemPrompt,
		Tools:                   agentTools,
		DisallowTransferToPeers: true,
	}

	if agentCfg.OutputKey != "" {
		llmCfg.OutputKey = agentCfg.OutputKey
	}

	return llmagent.New(llmCfg)
}

// BuildSkillsManifest fetches all skills from OpenSearch and returns a formatted
// <available_skills> block for injection into agent system prompts.
func BuildSkillsManifest(osClient *opensearch.Client) string {
	if osClient == nil {
		return ""
	}

	query := map[string]any{
		"size":    100,
		"_source": []string{"name", "description"},
		"query":   map[string]any{"match_all": map[string]any{}},
	}

	resp, err := osClient.Search(context.Background(), opensearch.IndexSkills, query)
	if err != nil || len(resp.Hits.Hits) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n<available_skills>\nUse the view_skill tool to load full instructions for any skill listed below.\n")
	for _, hit := range resp.Hits.Hits {
		var s struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(hit.Source, &s); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	b.WriteString("</available_skills>")
	return b.String()
}

func RequireSubAgent(cfg *config.Config, agentCfg *config.AgentConfig, name string, deps tools.Deps) (agent.Agent, error) {
	sub := agentCfg.FindSubAgent(name)
	if sub == nil {
		return nil, fmt.Errorf("required sub_agent %q not found in config", name)
	}
	a, err := BuildLLMAgent(cfg, sub, deps)
	if err != nil {
		return nil, fmt.Errorf("sub_agent %s: %w", name, err)
	}
	return a, nil
}
