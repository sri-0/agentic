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
	ID           string        `yaml:"id"`
	Type         string        `yaml:"type"`
	Name         string        `yaml:"name"`
	Description  string        `yaml:"description"`
	Model        string        `yaml:"model"`
	Provider     string        `yaml:"provider"`
	SystemPrompt string        `yaml:"system_prompt"`
	Tools        []string      `yaml:"tools"`
	SubAgents    []AgentConfig `yaml:"sub_agents,omitempty"`
	OutputKey    string        `yaml:"output_key,omitempty"`
	Keywords     []string      `yaml:"keywords,omitempty"`
}

// FindSubAgent returns the sub-agent config with the given name, or nil.
func (c *AgentConfig) FindSubAgent(name string) *AgentConfig {
	for i := range c.SubAgents {
		if c.SubAgents[i].Name == name {
			return &c.SubAgents[i]
		}
	}
	return nil
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
