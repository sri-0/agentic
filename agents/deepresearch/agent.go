package deepresearch

import (
	"fmt"

	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/rag"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/loopagent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
)

// NewAgent builds a deep research pipeline:
//
//	pipeline (Sequential)
//	  ├── research_planner (LLM: decomposes query into search strategy, OutputKey: research_plan)
//	  ├── rag_retrieval (code: searches RAG using plan, stores doc IDs in state)
//	  ├── document_loop (custom code agent: iterates over each document)
//	  │   per iteration:
//	  │   ├── document_fetcher (code: fetches next doc segments into state)
//	  │   ├── parallel_analysis (Parallel)
//	  │   │   ├── document_analyst (LLM, reads plan + document)
//	  │   │   └── data_analyst (LLM with DB tools, reads plan + document)
//	  │   ├── findings_synthesizer (LLM, OutputKey: current_findings)
//	  │   └── findings_accumulator (code: appends to all_findings, increments index)
//	  ├── gap_analyst (LLM: compares findings vs plan, OutputKey: research_gaps)
//	  └── report_refinement (LoopAgent, MaxIterations=3)
//	      ├── report_generator (LLM, reads plan + findings + gaps + feedback, OutputKey: draft_report)
//	      ├── report_critic (LLM, reviews draft, OutputKey: critic_feedback)
//	      └── quality_checker (code: escalates if critic approves)
//
// Note: document_loop is a custom agent rather than LoopAgent because ADK v0.5.0
// propagates escalation through LoopAgent to the parent SequentialAgent, which would
// stop gap_analyst and report_refinement from running. The report_refinement loop uses
// LoopAgent safely because it's the last step in the pipeline.
func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
	// ── Planning ──────────────────────────────────────────────────────────

	planner, err := shared.RequireSubAgent(cfg, agentCfg, "research_planner", ragClient)
	if err != nil {
		return nil, err
	}

	// ── RAG retrieval (code agent) ───────────────────────────────────────

	ragRetrieval, err := agent.New(agent.Config{
		Name: "rag_retrieval",
		Run:  ragRetrievalRun(ragClient),
	})
	if err != nil {
		return nil, fmt.Errorf("rag_retrieval: %w", err)
	}

	// ── Document analysis loop (custom agent) ────────────────────────────

	docAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "document_analyst", ragClient)
	if err != nil {
		return nil, err
	}

	dataAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "data_analyst", ragClient)
	if err != nil {
		return nil, err
	}

	parallelAnalysis, err := parallelagent.New(parallelagent.Config{
		AgentConfig: agent.Config{
			Name:      "parallel_analysis",
			SubAgents: []agent.Agent{docAnalyst, dataAnalyst},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("parallel_analysis: %w", err)
	}

	findingsSummarizer, err := shared.RequireSubAgent(cfg, agentCfg, "findings_summarizer", ragClient)
	if err != nil {
		return nil, err
	}

	// SubAgents must be registered so the ADK session service recognizes their events.
	// See the StoryFlowAgent example in ADK docs.
	docLoop, err := agent.New(agent.Config{
		Name:      "document_loop",
		SubAgents: []agent.Agent{parallelAnalysis, findingsSummarizer},
		Run:       documentLoopRun(ragClient, parallelAnalysis, findingsSummarizer),
	})
	if err != nil {
		return nil, fmt.Errorf("document_loop: %w", err)
	}

	// ── Gap analysis ─────────────────────────────────────────────────────

	gapAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "gap_analyst", ragClient)
	if err != nil {
		return nil, err
	}

	// ── Report refinement loop (generator → critic → quality check) ─────
	// LoopAgent is safe here — it's the last step, so escalation doesn't
	// cut off subsequent agents.

	reportGen, err := shared.RequireSubAgent(cfg, agentCfg, "report_generator", ragClient)
	if err != nil {
		return nil, err
	}

	reportCritic, err := shared.RequireSubAgent(cfg, agentCfg, "report_critic", ragClient)
	if err != nil {
		return nil, err
	}

	qualityChecker, err := agent.New(agent.Config{
		Name: "quality_checker",
		Run:  qualityCheckerRun(),
	})
	if err != nil {
		return nil, fmt.Errorf("quality_checker: %w", err)
	}

	reportLoop, err := loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:      "report_refinement",
			SubAgents: []agent.Agent{reportGen, reportCritic, qualityChecker},
		},
		MaxIterations: 3,
	})
	if err != nil {
		return nil, fmt.Errorf("report_refinement: %w", err)
	}

	// ── Full pipeline ────────────────────────────────────────────────────

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:      agentCfg.Name,
			SubAgents: []agent.Agent{planner, ragRetrieval, docLoop, gapAnalyst, reportLoop},
		},
	})
}
