package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type AgentsConfig struct {
	Agents []AgentConfig `yaml:"agents"`
}

type AgentConfig struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Model        string   `yaml:"model"`
	Provider     string   `yaml:"provider"`
	SystemPrompt string   `yaml:"system_prompt"`
	Tools        []string `yaml:"tools"`
}

func LoadAgents(path string) (*AgentsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading agents config: %w", err)
	}
	var cfg AgentsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing agents config: %w", err)
	}
	return &cfg, nil
}

// FindAgent returns the agent config with the given ID, or nil if not found.
func (c *AgentsConfig) FindAgent(id string) *AgentConfig {
	for i := range c.Agents {
		if c.Agents[i].ID == id {
			return &c.Agents[i]
		}
	}
	return nil
}

// AgentIDs returns all agent IDs.
func (c *AgentsConfig) AgentIDs() []string {
	ids := make([]string, len(c.Agents))
	for i, a := range c.Agents {
		ids[i] = a.ID
	}
	return ids
}
