package basic

import (
	"fmt"

	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/rag"
	genaiopenai "agentic/pkg/genai/openai"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
)

func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
	baseURL, apiKey, httpClient := shared.ResolveProvider(cfg, agentCfg)
	if baseURL == "" {
		return nil, fmt.Errorf("no provider for model %s", agentCfg.Model)
	}

	m := genaiopenai.New(genaiopenai.Config{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		ModelName:  agentCfg.Model,
		HTTPClient: httpClient,
	})

	agentTools, err := shared.ResolveTools(agentCfg.Tools, ragClient)
	if err != nil {
		return nil, err
	}

	return llmagent.New(llmagent.Config{
		Name:        agentCfg.Name,
		Description: agentCfg.Description,
		Model:       m,
		Instruction: agentCfg.SystemPrompt,
		Tools:       agentTools,
	})
}
