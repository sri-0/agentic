package deepresearch

import (
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"agentic/agents/shared"
	"agentic/internal/config"
	"agentic/internal/rag"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/loopagent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// NewAgent builds a deep research pipeline:
//
//	pipeline (Sequential)
//	  ├── rag_retrieval (code: searches RAG, stores doc IDs in state)
//	  ├── document_loop (Loop, MaxIterations=10)
//	  │   ├── document_fetcher (code: fetches next doc segments into state, escalates when done)
//	  │   ├── parallel_analysis (Parallel)
//	  │   │   ├── document_analyst (LLM, reads {current_document})
//	  │   │   └── data_analyst (LLM with DB tools, reads {current_document})
//	  │   ├── findings_summarizer (LLM, reads analysis + data, OutputKey: current_findings)
//	  │   └── findings_accumulator (code: appends current_findings to all_findings, increments index)
//	  └── report_generator (LLM, reads {all_findings})
func NewAgent(cfg *config.Config, agentCfg *config.AgentConfig, ragClient *rag.Client) (agent.Agent, error) {
	// Code agent: RAG retrieval
	ragRetrieval, err := agent.New(agent.Config{
		Name: "rag_retrieval",
		Run:  ragRetrievalRun(ragClient),
	})
	if err != nil {
		return nil, fmt.Errorf("rag_retrieval: %w", err)
	}

	// Code agent: fetches next document's segments into state
	docFetcher, err := agent.New(agent.Config{
		Name: "document_fetcher",
		Run:  docFetcherRun(ragClient),
	})
	if err != nil {
		return nil, fmt.Errorf("document_fetcher: %w", err)
	}

	// LLM sub-agents from YAML config
	docAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "document_analyst", ragClient)
	if err != nil {
		return nil, err
	}
	dataAnalyst, err := shared.RequireSubAgent(cfg, agentCfg, "data_analyst", ragClient)
	if err != nil {
		return nil, err
	}
	findingsSummarizer, err := shared.RequireSubAgent(cfg, agentCfg, "findings_summarizer", ragClient)
	if err != nil {
		return nil, err
	}
	reportGen, err := shared.RequireSubAgent(cfg, agentCfg, "report_generator", ragClient)
	if err != nil {
		return nil, err
	}

	// Code agent: accumulates per-document findings
	accumulator, err := agent.New(agent.Config{
		Name: "findings_accumulator",
		Run:  findingsAccumulatorRun(),
	})
	if err != nil {
		return nil, fmt.Errorf("findings_accumulator: %w", err)
	}

	// Parallel analysis per document
	parallelAnalysis, err := parallelagent.New(parallelagent.Config{
		AgentConfig: agent.Config{
			Name:      "parallel_analysis",
			SubAgents: []agent.Agent{docAnalyst, dataAnalyst},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("parallel_analysis: %w", err)
	}

	// Loop over documents
	docLoop, err := loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:      "document_loop",
			SubAgents: []agent.Agent{docFetcher, parallelAnalysis, findingsSummarizer, accumulator},
		},
		MaxIterations: 10,
	})
	if err != nil {
		return nil, fmt.Errorf("document_loop: %w", err)
	}

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:      agentCfg.Name,
			SubAgents: []agent.Agent{ragRetrieval, docLoop, reportGen},
		},
	})
}

// ragRetrievalRun searches RAG and stores document IDs + count in session state.
func ragRetrievalRun(ragClient *rag.Client) func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			query := extractUserQuery(ctx)
			docs, err := ragClient.Search(query, 5, nil)
			if err != nil {
				yield(nil, fmt.Errorf("rag search: %w", err))
				return
			}

			var docIDs []string
			var titles []string
			for _, doc := range docs {
				docIDs = append(docIDs, doc.Metadata.DocumentID)
				titles = append(titles, doc.Title)
			}

			docIDsJSON, _ := json.Marshal(docIDs)

			evt := session.NewEvent(ctx.InvocationID())
			evt.Author = "rag_retrieval"
			evt.LLMResponse = model.LLMResponse{
				Content: genai.NewContentFromText(
					fmt.Sprintf("Retrieved %d documents:\n- %s", len(docs), strings.Join(titles, "\n- ")),
					genai.RoleModel,
				),
			}
			evt.Actions.StateDelta = map[string]any{
				"doc_ids":      string(docIDsJSON),
				"doc_count":    len(docs),
				"doc_index":    0,
				"all_findings": "",
			}
			yield(evt, nil)
		}
	}
}

// docFetcherRun reads the next doc ID from state, fetches all segments, and stores in state.
// Escalates when all documents have been processed.
func docFetcherRun(ragClient *rag.Client) func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			docIndex := stateInt(ctx, "doc_index")
			docCount := stateInt(ctx, "doc_count")

			if docIndex >= docCount {
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "document_fetcher"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText("All documents processed.", genai.RoleModel),
				}
				evt.Actions.Escalate = true
				yield(evt, nil)
				return
			}

			var docIDs []string
			if raw := stateString(ctx, "doc_ids"); raw != "" {
				json.Unmarshal([]byte(raw), &docIDs)
			}
			if docIndex >= len(docIDs) {
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "document_fetcher"
				evt.Actions.Escalate = true
				yield(evt, nil)
				return
			}

			docID := docIDs[docIndex]
			doc, err := ragClient.GetByID(docID)
			if err != nil {
				yield(nil, fmt.Errorf("fetch doc %s: %w", docID, err))
				return
			}

			segments, err := ragClient.GetSegments(docID)
			if err != nil {
				yield(nil, fmt.Errorf("fetch segments %s: %w", docID, err))
				return
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "Document: %s\n", doc.Title)
			fmt.Fprintf(&sb, "ID: %s\n", doc.Metadata.DocumentID)
			fmt.Fprintf(&sb, "Source: %s\n", doc.Metadata.Source)
			fmt.Fprintf(&sb, "Author: %s\n", doc.Metadata.Author)
			fmt.Fprintf(&sb, "Date: %s\n", doc.Metadata.Date)
			fmt.Fprintf(&sb, "Classification: %s\n", doc.Metadata.Classification)
			fmt.Fprintf(&sb, "\nSegments (%d):\n", len(segments))
			for _, seg := range segments {
				fmt.Fprintf(&sb, "%d. %s\n", seg.SegmentID, seg.Content)
			}

			docContent := sb.String()

			evt := session.NewEvent(ctx.InvocationID())
			evt.Author = "document_fetcher"
			evt.LLMResponse = model.LLMResponse{
				Content: genai.NewContentFromText(
					fmt.Sprintf("Fetched document %d/%d: %s", docIndex+1, docCount, doc.Title),
					genai.RoleModel,
				),
			}
			evt.Actions.StateDelta = map[string]any{
				"current_document": docContent,
			}
			yield(evt, nil)
		}
	}
}

// findingsAccumulatorRun appends current_findings to all_findings and increments doc_index.
func findingsAccumulatorRun() func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			docIndex := stateInt(ctx, "doc_index")
			currentFindings := stateString(ctx, "current_findings")
			allFindings := stateString(ctx, "all_findings")

			if currentFindings != "" {
				if allFindings != "" {
					allFindings += "\n\n---\n\n"
				}
				allFindings += fmt.Sprintf("=== Document %d ===\n%s", docIndex+1, currentFindings)
			}

			evt := session.NewEvent(ctx.InvocationID())
			evt.Author = "findings_accumulator"
			evt.LLMResponse = model.LLMResponse{
				Content: genai.NewContentFromText(
					fmt.Sprintf("Accumulated findings for document %d.", docIndex+1),
					genai.RoleModel,
				),
			}
			evt.Actions.StateDelta = map[string]any{
				"doc_index":    docIndex + 1,
				"all_findings": allFindings,
			}
			yield(evt, nil)
		}
	}
}

func extractUserQuery(ctx agent.InvocationContext) string {
	if uc := ctx.UserContent(); uc != nil {
		for _, p := range uc.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return "general research"
}

func stateGet(ctx agent.InvocationContext, key string) any {
	v, _ := ctx.Session().State().Get(key)
	return v
}

func stateInt(ctx agent.InvocationContext, key string) int {
	switch v := stateGet(ctx, key).(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

func stateString(ctx agent.InvocationContext, key string) string {
	v, _ := stateGet(ctx, key).(string)
	return v
}
