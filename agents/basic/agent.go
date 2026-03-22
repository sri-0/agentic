package basic

import (
	"fmt"

	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/tools"
	genaiopenai "agentic/pkg/genai/openai"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
)

func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, deps tools.Deps) (agent.Agent, error) {
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

	agentTools, err := shared.ResolveTools(agentCfg.Tools, deps)
	if err != nil {
		return nil, err
	}

	instruction := agentCfg.SystemPrompt
	if manifest := shared.BuildSkillsManifest(deps.OSClient); manifest != "" {
		instruction += manifest
	}

	return llmagent.New(llmagent.Config{
		Name:        agentCfg.Name,
		Description: agentCfg.Description,
		Model:       m,
		Instruction: instruction,
		Tools:       agentTools,
	})
}
