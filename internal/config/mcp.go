package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// MCPConfig is the loaded set of MCP servers (config/<env>/mcp.yaml).
type MCPConfig struct {
	Servers map[string]MCPServerConfig `yaml:"servers"`
}

// MCPServerConfig describes one MCP server the backend connects to as a client.
type MCPServerConfig struct {
	Type    string            `yaml:"type"`              // "remote" (streamable HTTP) | "local" (stdio)
	URL     string            `yaml:"url,omitempty"`     // remote endpoint
	Command []string          `yaml:"command,omitempty"` // local stdio command + args
	Headers map[string]string `yaml:"headers,omitempty"` // static auth headers (API keys); supports ${ENV}
	OAuth   bool              `yaml:"oauth,omitempty"`   // backend-held OAuth (browser redirects; token stored server-side)
	Enabled *bool             `yaml:"enabled,omitempty"`

	// OAuth provider settings (only used when OAuth is true). All optional: if
	// AuthorizeURL/TokenURL are empty they are discovered from the server's
	// RFC 8414 metadata; if ClientID is empty, RFC 7591 dynamic client
	// registration is attempted.
	AuthorizeURL string   `yaml:"authorize_url,omitempty"`
	TokenURL     string   `yaml:"token_url,omitempty"`
	RegisterURL  string   `yaml:"register_url,omitempty"` // RFC 7591 dynamic registration endpoint
	ClientID     string   `yaml:"client_id,omitempty"`    // supports ${ENV}
	ClientSecret string   `yaml:"client_secret,omitempty"` // supports ${ENV}
	Scopes       []string `yaml:"scopes,omitempty"`
}

// IsEnabled reports whether the server is enabled (defaults true).
func (s MCPServerConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// envVarPattern matches ${NAME} references for expansion in config values.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces every ${NAME} in s with os.Getenv("NAME"). An undefined
// variable expands to the empty string (same semantics as os.Expand). This is
// the promised ${ENV} expansion for MCP header values (H3b) and OAuth client
// credentials.
func ExpandEnv(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := envVarPattern.FindStringSubmatch(match)[1]
		return os.Getenv(name)
	})
}

// ExpandedHeaders returns Headers with every value passed through ExpandEnv, so
// e.g. `Authorization: "Bearer ${OFFICE_MCP_KEY}"` becomes the resolved token.
// Header keys are left verbatim.
func (s MCPServerConfig) ExpandedHeaders() map[string]string {
	if len(s.Headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.Headers))
	for k, v := range s.Headers {
		out[k] = ExpandEnv(v)
	}
	return out
}

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
