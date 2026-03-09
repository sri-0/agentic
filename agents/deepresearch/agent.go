package deepresearch

import (
	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/rag"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
)

// NewAgent builds a deep research pipeline:
//
//	research_pipeline (Sequential)
//	  ├── rag_retrieval_agent (LLM with opensearch tools, OutputKey: "retrieved_documents")
//	  ├── parallel_analysis (Parallel)
//	  │   ├── document_analyst (LLM with summarize/extract tools, OutputKey: "analysis_findings")
//	  │   └── data_analyst (LLM with DB query tools, OutputKey: "database_results")
//	  └── report_generator (LLM with generate_report tool, OutputKey: "final_report")
func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
	ragAgent, err := shared.RequireSubAgent(cfg, agentCfg, "rag_retrieval_agent", ragClient)
	if err != nil {
		return nil, err
	}

	docAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "document_analyst", ragClient)
	if err != nil {
		return nil, err
	}

	dataAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "data_analyst", ragClient)
	if err != nil {
		return nil, err
	}

	reportGen, err := shared.RequireSubAgent(cfg, agentCfg, "report_generator", ragClient)
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
		return nil, err
	}

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:      agentCfg.Name,
			SubAgents: []agent.Agent{ragAgent, parallelAnalysis, reportGen},
		},
	})
}
