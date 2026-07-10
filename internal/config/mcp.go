package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// MCPConfig is the loaded set of MCP servers (config/<env>/mcp.yaml).
type MCPConfig struct {
	Servers map[string]MCPServerConfig `yaml:"servers"`
}

// MCPServerConfig describes one MCP server the backend connects to as a client.
type MCPServerConfig struct {
	Type    string            `yaml:"type"`            // "remote" (streamable HTTP) | "local" (stdio)
	URL     string            `yaml:"url,omitempty"`   // remote endpoint
	Command []string          `yaml:"command,omitempty"` // local stdio command + args
	Headers map[string]string `yaml:"headers,omitempty"` // static auth headers (API keys); supports ${ENV}
	OAuth   bool              `yaml:"oauth,omitempty"`   // backend-held OAuth (browser redirects; token stored server-side)
	Enabled *bool             `yaml:"enabled,omitempty"`
}

// IsEnabled reports whether the server is enabled (defaults true).
func (s MCPServerConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// LoadMCP loads mcp.yaml. A missing file yields an empty config (not an error).
func LoadMCP(path string) (*MCPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &MCPConfig{Servers: map[string]MCPServerConfig{}}, nil
		}
		return nil, fmt.Errorf("reading mcp config: %w", err)
	}
	var cfg MCPConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing mcp config: %w", err)
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]MCPServerConfig{}
	}
	return &cfg, nil
}
