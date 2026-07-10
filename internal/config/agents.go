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
	// Mode selects roster visibility: "primary" (user-selectable), "subagent"
	// (only dispatchable via the task tool), or "all". Empty defaults to primary
	// (or subagent when Internal is set).
	Mode         string        `yaml:"mode,omitempty"`
	Name         string        `yaml:"name"`
	Description  string        `yaml:"description"`
	Model        string        `yaml:"model"`
	Provider     string        `yaml:"provider"`
	SystemPrompt string   `yaml:"system_prompt"`
	Tools        []string `yaml:"tools"`
	// SubAgents is a list of agent IDs (roster references). Resolve via
	// AgentsConfig.ResolveSubAgents / ResolveSubAgentByName.
	SubAgents          []string `yaml:"sub_agents,omitempty"`
	Internal           bool     `yaml:"internal,omitempty"`
	OutputKey          string   `yaml:"output_key,omitempty"`
	OutputAgent        string   `yaml:"output_agent,omitempty"`
	Keywords           []string `yaml:"keywords,omitempty"`
	MaxIterations      int      `yaml:"max_iterations,omitempty"`
	MaxParallelWorkers int      `yaml:"max_parallel_workers,omitempty"`

	// AllowedSubagents restricts which subagent types this agent's task tool may
	// dispatch (governance). Empty means "all dispatchable" (legacy behaviour).
	AllowedSubagents []string `yaml:"allowed_subagents,omitempty"`

	// Leaf behaviour flags (consolidated from the former explore/plan/
	// verification/codeguide packages). Defaulted by agent type in
	// bootstrap.applyLeafDefaults when not set explicitly in YAML.
	ReadOnly             bool `yaml:"read_only,omitempty"`              // strip state-mutating tools (roster.ReadOnlyPermissions)
	AppendVerdict        bool `yaml:"append_verdict,omitempty"`         // append the PASS/FAIL/PARTIAL reminder
	InjectSkillsManifest bool `yaml:"inject_skills_manifest,omitempty"` // append the <available_skills> block

	// MCPServers lists MCP server names (from mcp.yaml) whose tools this agent
	// may use. Resolved to ADK toolsets at build time.
	MCPServers []string `yaml:"mcp_servers,omitempty"`

	// Per-request model override, set by WithModelOverride and propagated to
	// every resolved sub-agent so the user-selected model is used across the
	// whole tree (root + sub-agents). Not serialized.
	overrideModel    string
	overrideProvider string
}

// WithModelOverride returns a deep copy of c with Model and Provider set to the
// given values on the root agent, and records the override so it propagates to
// every resolved sub-agent (see ResolveSubAgents). The receiver is not mutated.
func (c *AgentConfig) WithModelOverride(modelID, provider string) *AgentConfig {
	if c == nil {
		return nil
	}
	cp := c.clone()
	cp.Model = modelID
	cp.Provider = provider
	cp.overrideModel = modelID
	cp.overrideProvider = provider
	return cp
}

// Clone returns a deep copy of the AgentConfig (exported wrapper over clone).
func (c *AgentConfig) Clone() *AgentConfig { return c.clone() }

// clone returns a deep copy of the AgentConfig, copying all slices so the
// original is never aliased.
func (c *AgentConfig) clone() *AgentConfig {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Tools = append([]string(nil), c.Tools...)
	cp.Keywords = append([]string(nil), c.Keywords...)
	cp.SubAgents = append([]string(nil), c.SubAgents...)
	cp.AllowedSubagents = append([]string(nil), c.AllowedSubagents...)
	return &cp
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

// ResolveSubAgents resolves each id in parent.SubAgents against the roster
// (via FindAgent) and returns the resolved configs (including internal ones).
// It errors if any referenced id is missing.
func (c *AgentsConfig) ResolveSubAgents(parent *AgentConfig) ([]*AgentConfig, error) {
	resolved := make([]*AgentConfig, 0, len(parent.SubAgents))
	for _, id := range parent.SubAgents {
		sub := c.FindAgent(id)
		if sub == nil {
			return nil, fmt.Errorf("sub_agent %q referenced by %q not found in roster", id, parent.ID)
		}
		// Propagate a per-request model override to every sub-agent so the
		// user-selected model is used across the whole tree.
		if parent.overrideModel != "" {
			oc := sub.clone()
			oc.Model = parent.overrideModel
			oc.Provider = parent.overrideProvider
			oc.overrideModel = parent.overrideModel
			oc.overrideProvider = parent.overrideProvider
			sub = oc
		}
		resolved = append(resolved, sub)
	}
	return resolved, nil
}

// ResolveSubAgentByName resolves parent.SubAgents against the roster and returns
// the one whose Name == name OR ID == name. It errors if no match is found.
func (c *AgentsConfig) ResolveSubAgentByName(parent *AgentConfig, name string) (*AgentConfig, error) {
	resolved, err := c.ResolveSubAgents(parent)
	if err != nil {
		return nil, err
	}
	for _, sub := range resolved {
		if sub.Name == name || sub.ID == name {
			return sub, nil
		}
	}
	return nil, fmt.Errorf("sub_agent %q not found in sub_agents of %q", name, parent.ID)
}

// AgentIDs returns all agent IDs.
func (c *AgentsConfig) AgentIDs() []string {
	ids := make([]string, len(c.Agents))
	for i, a := range c.Agents {
		ids[i] = a.ID
	}
	return ids
}
