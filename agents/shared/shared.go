package shared

import (
	"fmt"
	"net/http"

	"agentic/internal/config"
	"agentic/internal/rag"
	"agentic/internal/tools"
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

func ResolveTools(names []string, ragClient *rag.Client) ([]tool.Tool, error) {
	var resolved []tool.Tool
	for _, name := range names {
		t, err := tools.NewToolByName(name, ragClient)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", name, err)
		}
		resolved = append(resolved, t)
	}
	return resolved, nil
}

func BuildLLMAgent(cfg *config.Config, agentCfg *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
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

	agentTools, err := ResolveTools(agentCfg.Tools, ragClient)
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

func RequireSubAgent(cfg *config.Config, agentCfg *config.AgentConfig, name string, ragClient *rag.Client) (agent.Agent, error) {
	sub := agentCfg.FindSubAgent(name)
	if sub == nil {
		return nil, fmt.Errorf("required sub_agent %q not found in config", name)
	}
	a, err := BuildLLMAgent(cfg, sub, ragClient)
	if err != nil {
		return nil, fmt.Errorf("sub_agent %s: %w", name, err)
	}
	return a, nil
}
