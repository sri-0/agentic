// Package mcp connects the gateway to MCP servers as a client and exposes their
// tools to agents. The backend is the MCP client (not the browser), so tools and
// credentials live server-side — required because swarm runs continue in the
// background after a browser disconnects (see Phase 04 plan).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"agentic/internal/config"

	"github.com/rs/zerolog"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Status is the connection state reported to the UI for one MCP server.
type Status string

const (
	StatusConnected Status = "connected"  // toolset built; tools reachable
	StatusDisabled  Status = "disabled"   // enabled: false in config
	StatusFailed    Status = "failed"     // bad config/transport; server skipped
	StatusNeedsAuth Status = "needs_auth" // oauth server with no valid token for the user
)

// ServerStatus is the per-server status surfaced by GET /v1/mcp.
type ServerStatus struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	OAuth  bool   `json:"oauth"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Manager holds a toolset per configured MCP server, keyed by server name, plus
// the OAuth provider and per-server config used for auth and status reporting.
type Manager struct {
	logger   zerolog.Logger
	oauth    *OAuthProvider
	store    TokenStore
	mu       sync.RWMutex
	toolsets map[string]tool.Toolset
	configs  map[string]config.MCPServerConfig
	baseline map[string]Status // build-time status (connected/failed/disabled)
}

// NewManager builds toolsets for every enabled server. Connection is lazy (the
// adk toolset connects on first Tools() call), so a misconfigured/unreachable
// server does not block startup. NewManager NEVER panics on bad config: a broken
// server degrades to StatusFailed and is skipped (H3c).
//
// redirectURL is the gateway's public OAuth callback URL (GET
// /v1/mcp/oauth/callback); it may be empty when OAuth is not in use.
func NewManager(cfg *config.MCPConfig, redirectURL string, logger zerolog.Logger) *Manager {
	store := NewMemoryTokenStore()
	m := &Manager{
		logger:   logger,
		oauth:    NewOAuthProvider(store, redirectURL),
		store:    store,
		toolsets: map[string]tool.Toolset{},
		configs:  map[string]config.MCPServerConfig{},
		baseline: map[string]Status{},
	}
	if cfg == nil {
		return m
	}
	for name, sc := range cfg.Servers {
		m.configs[name] = sc
		if !sc.IsEnabled() {
			m.baseline[name] = StatusDisabled
			continue
		}
		transport, err := m.transportFor(name, sc)
		if err != nil {
			m.baseline[name] = StatusFailed
			logger.Warn().Err(err).Str("server", name).Msg("mcp: skipping server (bad transport)")
			continue
		}
		ts, err := mcptoolset.New(mcptoolset.Config{Transport: transport})
		if err != nil {
			m.baseline[name] = StatusFailed
			logger.Warn().Err(err).Str("server", name).Msg("mcp: toolset build failed")
			continue
		}
		m.toolsets[name] = ts
		m.baseline[name] = StatusConnected
		logger.Info().Str("server", name).Str("type", sc.Type).Bool("oauth", sc.OAuth).Msg("mcp: server registered")
	}
	return m
}

// Toolsets returns the toolsets for the named servers (unknown names skipped).
func (m *Manager) Toolsets(servers []string) []tool.Toolset {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
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
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.configs))
	for n := range m.configs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// OAuth exposes the provider so the connect/callback HTTP handlers can drive the
// flow.
func (m *Manager) OAuth() *OAuthProvider { return m.oauth }

// Config returns the config for a named server (ok=false if unknown).
func (m *Manager) Config(name string) (config.MCPServerConfig, bool) {
	if m == nil {
		return config.MCPServerConfig{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sc, ok := m.configs[name]
	return sc, ok
}

// Statuses returns the status of every configured server for the given user. For
// oauth servers the baseline status is upgraded/downgraded per whether the user
// holds a valid token (needs_auth vs connected).
func (m *Manager) Statuses(ctx context.Context, userID string) []ServerStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	names := make([]string, 0, len(m.configs))
	for n := range m.configs {
		names = append(names, n)
	}
	baseline := make(map[string]Status, len(m.baseline))
	for k, v := range m.baseline {
		baseline[k] = v
	}
	m.mu.RUnlock()
	sort.Strings(names)

	out := make([]ServerStatus, 0, len(names))
	for _, name := range names {
		sc, _ := m.Config(name)
		st := baseline[name]
		detail := ""
		if sc.OAuth && st == StatusConnected {
			// oauth servers need a per-user token; without one, needs_auth.
			if _, err := m.oauth.TokenFor(ctx, userID, name, sc); err != nil {
				st = StatusNeedsAuth
				detail = "no valid token for user; connect required"
			}
		}
		out = append(out, ServerStatus{
			Name:   name,
			Type:   sc.Type,
			OAuth:  sc.OAuth,
			Status: st,
			Detail: detail,
		})
	}
	return out
}

func (m *Manager) transportFor(name string, sc config.MCPServerConfig) (mcpsdk.Transport, error) {
	switch sc.Type {
	case "local":
		// stdio child process. Validate the command so a missing/empty `command:`
		// is a skipped server, never an index-out-of-range panic at startup (H3c).
		if len(sc.Command) == 0 || strings.TrimSpace(sc.Command[0]) == "" {
			return nil, errors.New("local server has empty command")
		}
		//nolint:gosec // command comes from operator config
		cmd := exec.Command(sc.Command[0], sc.Command[1:]...)
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	case "remote", "":
		// Streamable HTTP. Static-header (API-key) auth (H3a) and per-user
		// backend-held OAuth tokens are injected by a custom HTTPClient whose
		// RoundTripper adds them to every request.
		if sc.URL == "" {
			return nil, errors.New("remote server has empty url")
		}
		client := newAuthHTTPClient(name, sc, m.oauth)
		return &mcpsdk.StreamableClientTransport{Endpoint: sc.URL, HTTPClient: client}, nil
	default:
		return nil, fmt.Errorf("unknown server type %q", sc.Type)
	}
}
