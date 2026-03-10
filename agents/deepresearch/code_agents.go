package deepresearch

import (
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"agentic/internal/rag"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// ragRetrievalRun searches RAG using the research plan (if available) or user query.
// Stores document IDs, count, and initializes loop state.
func ragRetrievalRun(ragClient *rag.Client) func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			// Use research plan for better search queries, fall back to raw user query
			query := stateString(ctx, KeyResearchPlan)
			if query == "" {
				query = extractUserQuery(ctx)
			}

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
				KeyDocIDs:          string(docIDsJSON),
				KeyDocCount:        len(docs),
				KeyDocIndex:        0,
				KeyAllFindings:     "",
				KeyCurrentDocument: "",
				KeyCurrentFindings: "",
				KeyResearchGaps:    "",
				KeyDraftReport:     "",
				KeyCriticFeedback:  "",
			}
			yield(evt, nil)
		}
	}
}

// documentLoopRun is a custom agent that iterates over each document, running
// sub-agents per document: fetch → parallel analysis → synthesize → accumulate.
//
// This follows the ADK custom agent pattern (see StoryFlowAgent example) where
// sub-agents are invoked via subAgent.Run(ctx) inside the custom Run function.
// This avoids the LoopAgent escalation propagation issue in ADK v0.5.0 where
// escalation from a LoopAgent propagates to the parent SequentialAgent.
func documentLoopRun(
	ragClient *rag.Client,
	parallelAnalysis agent.Agent,
	findingsSummarizer agent.Agent,
) func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			docCount := stateInt(ctx, KeyDocCount)

			var docIDs []string
			if raw := stateString(ctx, KeyDocIDs); raw != "" {
				json.Unmarshal([]byte(raw), &docIDs)
			}

			maxDocs := docCount
			if maxDocs > len(docIDs) {
				maxDocs = len(docIDs)
			}
			if maxDocs > 10 { // safety cap
				maxDocs = 10
			}

			for i := 0; i < maxDocs; i++ {
				// ── Fetch document ────────────────────────────
				doc, err := ragClient.GetByID(docIDs[i])
				if err != nil {
					yield(nil, fmt.Errorf("fetch doc %s: %w", docIDs[i], err))
					return
				}

				segments, err := ragClient.GetSegments(docIDs[i])
				if err != nil {
					yield(nil, fmt.Errorf("fetch segments %s: %w", docIDs[i], err))
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

				// Emit fetch progress event
				fetchEvt := session.NewEvent(ctx.InvocationID())
				fetchEvt.Author = "document_fetcher"
				fetchEvt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText(
						fmt.Sprintf("Fetched document %d/%d: %s", i+1, maxDocs, doc.Title),
						genai.RoleModel,
					),
				}
				fetchEvt.Actions.StateDelta = map[string]any{
					KeyCurrentDocument: sb.String(),
					KeyDocIndex:        i,
				}
				if !yield(fetchEvt, nil) {
					return
				}

				// ── Run parallel analysis (document_analyst + data_analyst) ──
				for event, err := range parallelAnalysis.Run(ctx) {
					if err != nil {
						yield(nil, fmt.Errorf("parallel analysis doc %d: %w", i+1, err))
						return
					}
					if !yield(event, nil) {
						return
					}
				}

				// ── Run findings synthesizer ─────────────────
				for event, err := range findingsSummarizer.Run(ctx) {
					if err != nil {
						yield(nil, fmt.Errorf("findings synthesizer doc %d: %w", i+1, err))
						return
					}
					if !yield(event, nil) {
						return
					}
				}

				// ── Accumulate findings ──────────────────────
				currentFindings := stateString(ctx, KeyCurrentFindings)
				allFindings := stateString(ctx, KeyAllFindings)

				if currentFindings != "" {
					if allFindings != "" {
						allFindings += "\n\n---\n\n"
					}
					allFindings += fmt.Sprintf("=== Document %d ===\n%s", i+1, currentFindings)
				}

				accumEvt := session.NewEvent(ctx.InvocationID())
				accumEvt.Author = "findings_accumulator"
				accumEvt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText(
						fmt.Sprintf("Accumulated findings for document %d.", i+1),
						genai.RoleModel,
					),
				}
				accumEvt.Actions.StateDelta = map[string]any{
					KeyDocIndex:    i + 1,
					KeyAllFindings: allFindings,
				}
				if !yield(accumEvt, nil) {
					return
				}
			}

			// Emit completion event
			doneEvt := session.NewEvent(ctx.InvocationID())
			doneEvt.Author = "document_loop"
			doneEvt.LLMResponse = model.LLMResponse{
				Content: genai.NewContentFromText(
					fmt.Sprintf("All %d documents processed.", maxDocs),
					genai.RoleModel,
				),
			}
			if !yield(doneEvt, nil) {
				return
			}
		}
	}
}

// qualityCheckerRun reads the critic's feedback and escalates if the report is approved.
// Used inside the report_refinement LoopAgent (safe because it's the last pipeline step).
func qualityCheckerRun() func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			feedback := stateString(ctx, KeyCriticFeedback)
			upper := strings.ToUpper(feedback)

			approved := strings.Contains(upper, "APPROVED") ||
				strings.Contains(upper, "NO MAJOR ISSUES") ||
				strings.Contains(upper, "REPORT IS COMPLETE")

			evt := session.NewEvent(ctx.InvocationID())
			evt.Author = "quality_checker"

			if approved {
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText("Report approved by critic. Finalizing.", genai.RoleModel),
				}
				evt.Actions.Escalate = true
			} else {
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText("Report needs revision. Continuing refinement.", genai.RoleModel),
				}
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
