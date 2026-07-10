package coordinator

import (
	"fmt"

	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/tools"
	genaiopenai "agentic/pkg/genai/openai"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/workflowagents/loopagent"
	"google.golang.org/genai"
)

// NewAgent builds a coordinator agent modeled after Claude Code's coordinator mode.
//
// The coordinator orchestrates software engineering tasks across multiple workers.
// It does NOT execute tools directly — it delegates all research, implementation,
// and verification to worker sub-agents via a task board.
//
// Architecture (LoopAgent):
//
//	coordinator_loop (LoopAgent, MaxIterations from config)
//	  ├── coordinator (LLM: plans tasks, reviews results, synthesizes)
//	  ├── dispatch (code agent: runs pending tasks via worker agents in parallel)
//	  └── check_tasks (code agent: checks if done, escalates if synthesis ready)
//
// Workers are declared as sub_agents in agents.yaml and run inside the dispatch agent.
// This mirrors the swarm pattern but with Claude Code's coordinator system prompt
// which emphasizes synthesis, prompt quality, and the continue-vs-spawn decision.
func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, deps tools.Deps) (agent.Agent, error) {
	logger := deps.Logger.With().Str("agent_type", "coordinator").Logger()

	maxIterations := agentCfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 5
	}
	maxParallel := agentCfg.MaxParallelWorkers
	if maxParallel <= 0 {
		maxParallel = 3
	}

	// Build worker agents from sub_agents config
	workerAgents := make(map[string]agent.Agent)
	var workerInfos []workerInfo
	var workerAgentList []agent.Agent

	resolved, err := cfg.Agents.ResolveSubAgents(agentCfg)
	if err != nil {
		return nil, err
	}
	for _, sub := range resolved {
		w, err := shared.BuildLLMAgent(cfg, sub, deps)
		if err != nil {
			return nil, fmt.Errorf("worker %s: %w", sub.Name, err)
		}
		workerAgents[sub.Name] = w
		workerAgentList = append(workerAgentList, w)
		workerInfos = append(workerInfos, workerInfo{
			name:        sub.Name,
			description: sub.Description,
			tools:       sub.Tools,
		})
	}

	if len(workerAgents) == 0 {
		return nil, fmt.Errorf("coordinator requires at least one worker in sub_agents")
	}

	// Build coordinator LLM agent
	baseURL, apiKey, httpClient := shared.ResolveProvider(cfg, agentCfg)
	if baseURL == "" {
		return nil, fmt.Errorf("no provider for coordinator model %s", agentCfg.Model)
	}

	m := genaiopenai.New(genaiopenai.Config{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		ModelName:  agentCfg.Model,
		HTTPClient: httpClient,
	})

	workerManifest := buildWorkerManifest(workerInfos)

	coordinatorPrompt := agentCfg.SystemPrompt + "\n\n" + workerManifest + `

## How to coordinate

You control workers via a task board stored in session state.

### Output format (REQUIRED)
Always reply with a single JSON object of this exact shape — no prose, no markdown:
{"tasks": [{"id": "t1", "worker": "worker_name", "input": "what to do", "status": "pending", "result": ""}]}

- "worker" MUST be one of the available workers above.
- Always include ALL existing tasks (done + new pending). Only change status/result of completed tasks; never set a completed task back to "pending".
- The final deliverable is produced by a task assigned to the writer worker (it reads earlier results from the board). Do NOT write the answer yourself.
- Add a purpose statement to each task input so workers calibrate depth; launch independent workers in parallel.

### Current state
Task board: {coordinator:task_board}
Iteration: {coordinator:iteration}`

	coordinatorAgent, err := llmagent.New(llmagent.Config{
		Name:        "coordinator",
		Description: "Coordinator that plans tasks, assigns workers, synthesizes results, and directs follow-up work",
		Model:       m,
		Instruction: coordinatorPrompt,
		OutputKey:   KeyTaskBoard,
		GenerateContentConfig: &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("coordinator: %w", err)
	}

	// Build dispatch code agent (runs workers in parallel)
	dispatch, err := agent.New(agent.Config{
		Name:      "dispatch",
		SubAgents: workerAgentList,
		Run:       dispatchRun(workerAgents, maxParallel, agentCfg.OutputAgent, logger),
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}

	// Build check_tasks code agent
	checkTasks, err := agent.New(agent.Config{
		Name: "check_tasks",
		Run:  checkTasksRun(maxIterations, logger),
	})
	if err != nil {
		return nil, fmt.Errorf("check_tasks: %w", err)
	}

	// Build the coordinator loop: coordinator → dispatch → check_tasks → repeat
	return loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:      agentCfg.Name,
			SubAgents: []agent.Agent{coordinatorAgent, dispatch, checkTasks},
		},
		MaxIterations: uint(maxIterations),
	})
}
