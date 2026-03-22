package swarm

import (
	"fmt"

	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/tools"
	genaiopenai "agentic/pkg/genai/openai"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/workflowagents/loopagent"
)

// NewAgent builds a swarm agent:
//
//	swarm (LoopAgent, MaxIterations from config)
//	  ├── coordinator (LLM: plans tasks, reviews results, synthesizes)
//	  ├── dispatch (code agent: runs pending tasks via worker agents in parallel)
//	  └── check_tasks (code agent: checks if done, escalates if synthesis ready)
//
// The coordinator sees the task board in its system prompt via {swarm:task_board}
// and has access to the available_workers manifest. It outputs JSON task plans
// or a final synthesis.
//
// Workers are declared as sub_agents in agents.yaml and run inside the dispatch agent.
func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, deps tools.Deps) (agent.Agent, error) {
	logger := deps.Logger.With().Str("agent_type", "swarm").Logger()

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

	for i := range agentCfg.SubAgents {
		sub := &agentCfg.SubAgents[i]
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
		return nil, fmt.Errorf("swarm requires at least one worker in sub_agents")
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

You control a swarm of workers via a task board stored in session state.

### To assign tasks
Set the output_key "swarm:task_board" with a JSON array of tasks:
[{"id": "t1", "worker": "worker_name", "input": "what to do", "status": "pending", "result": ""}]

IMPORTANT: Always include ALL existing tasks (done + new pending) in the array. Only change the status/result of completed tasks.

### To add follow-up tasks
After reviewing results, add new tasks with status "pending" while keeping completed tasks.

### To finish
When you have enough information, set output_key "swarm:synthesis" to your final answer.
The synthesis should be a complete response to the user's original question.

### Current state
Task board: {swarm:task_board}
Iteration: {swarm:iteration}`

	coordinator, err := llmagent.New(llmagent.Config{
		Name:        "coordinator",
		Description: "Swarm coordinator that plans tasks, assigns workers, and synthesizes results",
		Model:       m,
		Instruction: coordinatorPrompt,
		OutputKey:   KeyTaskBoard,
	})
	if err != nil {
		return nil, fmt.Errorf("coordinator: %w", err)
	}

	// Build dispatch code agent (runs workers in parallel)
	dispatch, err := agent.New(agent.Config{
		Name:      "dispatch",
		SubAgents: workerAgentList,
		Run:       dispatchRun(workerAgents, maxParallel, logger),
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

	// Build the swarm loop: coordinator → dispatch → check_tasks → repeat
	return loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:      agentCfg.Name,
			SubAgents: []agent.Agent{coordinator, dispatch, checkTasks},
		},
		MaxIterations: uint(maxIterations),
	})
}
