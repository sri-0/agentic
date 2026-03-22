// cmd/dev runs the ADK web dev UI with all configured agents.
//
// Usage:
//
//	go run cmd/dev/main.go                          # default: web + api + webui on :8080
//	go run cmd/dev/main.go -agent test-agent        # single agent
//	go run cmd/dev/main.go -port 9090               # custom port
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"agentic/internal/bootstrap"
	"agentic/internal/logbridge"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
)

func main() {
	agentID := flag.String("agent", "", "Run a single agent by ID (default: all agents, first is root)")
	port := flag.Int("port", 8080, "Port for the ADK web UI")
	flag.Parse()

	ctx := context.Background()

	res, err := bootstrap.Init(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}

	loader, err := buildLoader(res, *agentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loader: %v\n", err)
		os.Exit(1)
	}

	logbridge.Setup(res.Logger)

	config := &launcher.Config{
		AgentLoader:    loader,
		SessionService: res.SessionService,
	}

	// Build args for the full launcher.
	// Default to "web api webui" which starts the dev UI + REST API.
	args := flag.Args()
	if len(args) == 0 {
		args = []string{"web", fmt.Sprintf("-port=%d", *port), "api", "webui"}
	}

	l := full.NewLauncher()
	if err := l.Execute(ctx, config, args); err != nil {
		log.Fatalf("launcher: %v\n\n%s", err, l.CommandLineSyntax())
	}
}

func buildLoader(res *bootstrap.Result, agentID string) (agent.Loader, error) {
	if agentID != "" {
		a, ok := res.Agents[agentID]
		if !ok {
			return nil, fmt.Errorf("agent %q not found (available: %v)", agentID, agentIDs(res))
		}
		return agent.NewSingleLoader(a), nil
	}

	// Multi-agent: first agent from config is root.
	var root agent.Agent
	var rest []agent.Agent
	for _, ac := range res.Cfg.Agents.Agents {
		a, ok := res.Agents[ac.ID]
		if !ok {
			continue
		}
		if root == nil {
			root = a
		} else {
			rest = append(rest, a)
		}
	}
	if root == nil {
		return nil, fmt.Errorf("no agents available")
	}
	if len(rest) == 0 {
		return agent.NewSingleLoader(root), nil
	}
	return agent.NewMultiLoader(root, rest...)
}

func agentIDs(res *bootstrap.Result) []string {
	ids := make([]string, 0, len(res.Agents))
	for id := range res.Agents {
		ids = append(ids, id)
	}
	return ids
}
