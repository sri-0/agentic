// Package mcp connects the gateway to MCP servers as a client and exposes their
// tools to agents. The backend is the MCP client (not the browser), so tools and
// credentials live server-side — required because swarm runs continue in the
// background after a browser disconnects (see Phase 04 plan).
package mcp

import (
	"os/exec"

	"agentic/internal/config"

	"github.com/rs/zerolog"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Manager holds a toolset per configured MCP server, keyed by server name.
type Manager struct {
	logger   zerolog.Logger
	toolsets map[string]tool.Toolset
}

// NewManager builds toolsets for every enabled server. Connection is lazy (the
// adk toolset connects on first Tools() call), so a misconfigured/unreachable
// server does not block startup.
func NewManager(cfg *config.MCPConfig, logger zerolog.Logger) *Manager {
	m := &Manager{logger: logger, toolsets: map[string]tool.Toolset{}}
	if cfg == nil {
		return m
	}
	for name, sc := range cfg.Servers {
		if !sc.IsEnabled() {
			continue
		}
		transport, err := transportFor(sc)
		if err != nil {
			logger.Warn().Err(err).Str("server", name).Msg("mcp: skipping server (bad transport)")
			continue
		}
		ts, err := mcptoolset.New(mcptoolset.Config{Transport: transport})
		if err != nil {
			logger.Warn().Err(err).Str("server", name).Msg("mcp: toolset build failed")
			continue
		}
		m.toolsets[name] = ts
		logger.Info().Str("server", name).Str("type", sc.Type).Bool("oauth", sc.OAuth).Msg("mcp: server registered")
	}
	return m
}

// Toolsets returns the toolsets for the named servers (unknown names skipped).
func (m *Manager) Toolsets(servers []string) []tool.Toolset {
	if m == nil {
		return nil
	}
	var out []tool.Toolset
	for _, s := range servers {
		if ts, ok := m.toolsets[s]; ok {
			out = append(out, ts)
		}
	}
	return out
}

// Names returns the configured server names.
func (m *Manager) Names() []string {
	names := make([]string, 0, len(m.toolsets))
	for n := range m.toolsets {
		names = append(names, n)
	}
	return names
}

func transportFor(sc config.MCPServerConfig) (mcpsdk.Transport, error) {
	switch sc.Type {
	case "local":
		// stdio child process.
		//nolint:gosec // command comes from operator config
		cmd := exec.Command(sc.Command[0], sc.Command[1:]...)
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	default: // "remote"
		// Streamable HTTP. Static-header (API-key) and OAuth-held token injection
		// are added via a custom HTTPClient RoundTripper (Phase 04 auth — see
		// plans/swarm/04-mcp.md); the base transport connects unauthenticated.
		return &mcpsdk.StreamableClientTransport{Endpoint: sc.URL}, nil
	}
}
