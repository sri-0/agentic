package triage

import (
	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/rag"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
)

// NewAgent builds a triage pipeline:
//
//	triage_pipeline (Sequential)
//	  ├── issue_extractor (LLM, OutputKey: "extracted_issue")
//	  ├── parallel_assessment (Parallel)
//	  │   ├── keyword_analyst (LLM with classify_incident, OutputKey: "keyword_analysis")
//	  │   ├── incident_researcher (LLM with get_incident_context/opensearch, OutputKey: "incident_research")
//	  │   └── severity_classifier (LLM with classify_incident, OutputKey: "severity_classification")
//	  └── report_agent (LLM with trigger_alert, OutputKey: "triage_report")
func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
	issueExtractor, err := shared.RequireSubAgent(cfg, agentCfg, "issue_extractor", ragClient)
	if err != nil {
		return nil, err
	}

	keywordAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "keyword_analyst", ragClient)
	if err != nil {
		return nil, err
	}

	incidentResearcher, err := shared.RequireSubAgent(cfg, agentCfg, "incident_researcher", ragClient)
	if err != nil {
		return nil, err
	}

	severityClassifier, err := shared.RequireSubAgent(cfg, agentCfg, "severity_classifier", ragClient)
	if err != nil {
		return nil, err
	}

	reportAgent, err := shared.RequireSubAgent(cfg, agentCfg, "report_agent", ragClient)
	if err != nil {
		return nil, err
	}

	parallelAssessment, err := parallelagent.New(parallelagent.Config{
		AgentConfig: agent.Config{
			Name:      "parallel_assessment",
			SubAgents: []agent.Agent{keywordAnalyst, incidentResearcher, severityClassifier},
		},
	})
	if err != nil {
		return nil, err
	}

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:      agentCfg.Name,
			SubAgents: []agent.Agent{issueExtractor, parallelAssessment, reportAgent},
		},
	})
}
