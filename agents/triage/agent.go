package triage

import (
	"fmt"

	"agentic/internal/config"
	"agentic/internal/rag"
	"agentic/internal/tools"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
)

// NewAgent builds the triage agent hierarchy from config.
func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
	// Build sub-agents from config
	var subAgents []agent.Agent

	for i := range agentCfg.SubAgents {
		sub := &agentCfg.SubAgents[i]
		sa, err := buildSubAgent(cfg, sub, ragClient)
		if err != nil {
			return nil, fmt.Errorf("building sub-agent %s: %w", sub.Name, err)
		}
		subAgents = append(subAgents, sa)
	}

	// Build orchestrator
	baseURL, apiKey := resolveProvider(cfg, agentCfg)
	if baseURL == "" {
		return nil, fmt.Errorf("no provider found for orchestrator model %s", agentCfg.Model)
	}

	orchModel := genaiopenai.New(genaiopenai.Config{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		ModelName: agentCfg.Model,
	})

	orchTools, err := resolveTools(agentCfg.Tools, ragClient)
	if err != nil {
		return nil, fmt.Errorf("resolving orchestrator tools: %w", err)
	}

	return llmagent.New(llmagent.Config{
		Name:        agentCfg.Name,
		Description: agentCfg.Description,
		Model:       orchModel,
		Instruction: agentCfg.SystemPrompt,
		Tools:       orchTools,
		SubAgents:   subAgents,
	})
}

func buildSubAgent(cfg *config.Config, sub *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
	baseURL, apiKey := resolveProvider(cfg, sub)
	if baseURL == "" {
		return nil, fmt.Errorf("no provider found for model %s", sub.Model)
	}

	m := genaiopenai.New(genaiopenai.Config{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		ModelName: sub.Model,
	})

	subTools, err := resolveTools(sub.Tools, ragClient)
	if err != nil {
		return nil, err
	}

	llmCfg := llmagent.Config{
		Name:                    sub.Name,
		Description:             sub.Description,
		Model:                   m,
		Instruction:             sub.SystemPrompt,
		Tools:                   subTools,
		DisallowTransferToPeers: true,
	}

	if sub.OutputKey != "" {
		llmCfg.OutputKey = sub.OutputKey
	}

	return llmagent.New(llmCfg)
}

func resolveProvider(cfg *config.Config, agentCfg *config.AgentConfig) (string, string) {
	if cfg.Models != nil && agentCfg.Provider != "" {
		if p := cfg.Models.FindProvider(agentCfg.Provider); p != nil {
			return p.BaseURL, p.APIKey()
		}
	}
	return "", ""
}

func resolveTools(names []string, ragClient *rag.Client) ([]tool.Tool, error) {
	var resolved []tool.Tool
	for _, name := range names {
		t, err := tools.NewToolByName(name, ragClient)
		if err != nil {
			return nil, fmt.Errorf("creating tool %s: %w", name, err)
		}
		resolved = append(resolved, t)
	}
	return resolved, nil
}
