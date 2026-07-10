package mcp

import (
	"context"
	"testing"

	"agentic/internal/config"

	"github.com/rs/zerolog"
)

// TestNewManager_NoPanicOnBadConfig proves H3c: a broken server config (empty
// command / empty url / unknown type) degrades to a skipped server and NEVER
// panics during NewManager (which runs inside bootstrap.Init).
func TestNewManager_NoPanicOnBadConfig(t *testing.T) {
	cfg := &config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"empty-command": {Type: "local", Command: nil},
		"blank-command": {Type: "local", Command: []string{"   "}},
		"empty-url":     {Type: "remote", URL: ""},
		"unknown-type":  {Type: "weird"},
		"disabled":      {Type: "remote", URL: "http://x", Enabled: boolPtr(false)},
		"good-local":    {Type: "local", Command: []string{"echo", "hi"}},
		"good-remote":   {Type: "remote", URL: "http://localhost:9999/mcp"},
	}}

	m := NewManager(cfg, "", zerolog.Nop()) // must not panic

	statuses := map[string]Status{}
	for _, s := range m.Statuses(context.Background(), "anonymous") {
		statuses[s.Name] = s.Status
	}
	if statuses["empty-command"] != StatusFailed {
		t.Errorf("empty-command: got %q want failed", statuses["empty-command"])
	}
	if statuses["blank-command"] != StatusFailed {
		t.Errorf("blank-command: got %q want failed", statuses["blank-command"])
	}
	if statuses["empty-url"] != StatusFailed {
		t.Errorf("empty-url: got %q want failed", statuses["empty-url"])
	}
	if statuses["unknown-type"] != StatusFailed {
		t.Errorf("unknown-type: got %q want failed", statuses["unknown-type"])
	}
	if statuses["disabled"] != StatusDisabled {
		t.Errorf("disabled: got %q want disabled", statuses["disabled"])
	}
	if statuses["good-local"] != StatusConnected {
		t.Errorf("good-local: got %q want connected", statuses["good-local"])
	}
	if statuses["good-remote"] != StatusConnected {
		t.Errorf("good-remote: got %q want connected", statuses["good-remote"])
	}
}

// TestManager_OAuthNeedsAuth proves an oauth server with no stored token reports
// needs_auth per user.
func TestManager_OAuthNeedsAuth(t *testing.T) {
	cfg := &config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"gitlab": {Type: "remote", URL: "http://localhost:9999/mcp", OAuth: true},
	}}
	m := NewManager(cfg, "", zerolog.Nop())
	st := m.Statuses(context.Background(), "alice")
	if len(st) != 1 || st[0].Status != StatusNeedsAuth {
		t.Fatalf("expected needs_auth, got %+v", st)
	}
}

func boolPtr(b bool) *bool { return &b }
