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
	OutputKey          string        `yaml:"output_key,omitempty"`
	OutputAgent        string        `yaml:"output_agent,omitempty"`
	Keywords           []string      `yaml:"keywords,omitempty"`
	MaxIterations      int           `yaml:"max_iterations,omitempty"`
	MaxParallelWorkers int           `yaml:"max_parallel_workers,omitempty"`
}

// WithModelOverride returns a deep copy of c with Model and Provider set to the
// given values on the root agent AND on every sub-agent recursively (any depth).
// The receiver is not mutated; all slices are copied so the original config is
// untouched.
func (c *AgentConfig) WithModelOverride(modelID, provider string) *AgentConfig {
	if c == nil {
		return nil
	}
	cp := c.clone()
	cp.applyModelOverride(modelID, provider)
	return cp
}

// applyModelOverride sets Model/Provider on this config and all sub-agents.
func (c *AgentConfig) applyModelOverride(modelID, provider string) {
	c.Model = modelID
	c.Provider = provider
	for i := range c.SubAgents {
		c.SubAgents[i].applyModelOverride(modelID, provider)
	}
}

// clone returns a deep copy of the AgentConfig, copying all slices and
// recursively cloning sub-agents so the original is never aliased.
func (c *AgentConfig) clone() *AgentConfig {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Tools = append([]string(nil), c.Tools...)
	cp.Keywords = append([]string(nil), c.Keywords...)
	if len(c.SubAgents) > 0 {
		cp.SubAgents = make([]AgentConfig, len(c.SubAgents))
		for i := range c.SubAgents {
			cp.SubAgents[i] = *c.SubAgents[i].clone()
		}
	} else {
		cp.SubAgents = nil
	}
	return &cp
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
