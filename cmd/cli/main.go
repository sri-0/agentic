// cmd/cli runs an agent in interactive console mode.
//
// Usage:
//
//	go run cmd/cli/main.go                          # first agent from config
//	go run cmd/cli/main.go -agent test-agent        # specific agent
//	go run cmd/cli/main.go -agent triage-agent      # triage pipeline
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"agentic/internal/bootstrap"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
)

func main() {
	agentID := flag.String("agent", "", "Agent ID to run (default: first agent from config)")
	flag.Parse()

	ctx := context.Background()

	res, err := bootstrap.Init(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}

	a, err := pickAgent(res, *agentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Starting interactive session with %q\n\n", a.Name())

	config := &launcher.Config{
		AgentLoader:    agent.NewSingleLoader(a),
		SessionService: res.SessionService,
	}

	// Use the full launcher in console mode.
	args := append([]string{"console"}, flag.Args()...)
	l := full.NewLauncher()
	if err := l.Execute(ctx, config, args); err != nil {
		log.Fatalf("console: %v", err)
	}
}

func pickAgent(res *bootstrap.Result, agentID string) (agent.Agent, error) {
	if agentID != "" {
		a, ok := res.Agents[agentID]
		if !ok {
			ids := make([]string, 0, len(res.Agents))
			for id := range res.Agents {
				ids = append(ids, id)
			}
			return nil, fmt.Errorf("agent %q not found (available: %v)", agentID, ids)
		}
		return a, nil
	}

	// Default to first agent from config ordering.
	for _, ac := range res.Cfg.Agents.Agents {
		if a, ok := res.Agents[ac.ID]; ok {
			return a, nil
		}
	}
	return nil, fmt.Errorf("no agents available")
}
