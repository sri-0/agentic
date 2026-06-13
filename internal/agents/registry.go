// Package agents provides internal (non-user-facing) agents used for system
// operations like context compaction, session memory extraction, tool use
// summarization, and prompt suggestions.
//
// These agents use the ADK framework but are NOT registered in the user-facing
// agent registry. They share the same session service and model providers.
package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// InternalAgent wraps an ADK agent with its runner for single-turn invocations.
type InternalAgent struct {
	Agent  adkagent.Agent
	Runner *runner.Runner
	Name   string
}

// Registry holds internal agents keyed by name.
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*InternalAgent
}

// NewRegistry creates an empty internal agent registry.
func NewRegistry() *Registry {
	return &Registry{
		agents: make(map[string]*InternalAgent),
	}
}

// Register adds an internal agent to the registry.
func (r *Registry) Register(name string, a *InternalAgent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agents[name] = a
}

// Get returns the internal agent by name, or nil if not found.
func (r *Registry) Get(name string) *InternalAgent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[name]
}

// Run executes a single-turn agent invocation and collects the text output.
// It creates an ephemeral session, sends the input, and collects the response.
func (r *Registry) Run(ctx context.Context, name string, sessionSvc session.Service, userID, input string) (string, error) {
	ia := r.Get(name)
	if ia == nil {
		return "", fmt.Errorf("internal agent %q not found", name)
	}

	// Build user content
	content := genai.NewContentFromText(input, genai.RoleUser)

	// Use a unique session ID per invocation to avoid state leaking
	sessionID := fmt.Sprintf("internal-%s-%s", name, randomHex(8))

	// Pre-create the session — the ADK runner expects it to exist
	_, err := sessionSvc.Create(ctx, &session.CreateRequest{
		AppName:   "agentic-internal",
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return "", fmt.Errorf("creating session for %s: %w", name, err)
	}

	// Run the agent via the runner
	var output string
	for evt, err := range ia.Runner.Run(ctx, userID, sessionID, content, adkagent.RunConfig{}) {
		if err != nil {
			return "", fmt.Errorf("running %s: %w", name, err)
		}
		if evt.Content != nil {
			for _, p := range evt.Content.Parts {
				if p.Text != "" {
					output = p.Text
				}
			}
		}
	}

	return output, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
