package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"agentic/internal/sse"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

// StreamAgentRun uses the ADK runner with StreamingModeSSE to execute the
// agent loop, mapping ADK events to OpenAI-compatible SSE chunks.
func StreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, messages []types.ChatMessage, logger zerolog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	cb := sse.NewChunkBuilder(requestID, core.AgentID, threadID)

	logger.Info().Str("thread_id", threadID).Str("agent_id", core.AgentID).Int("messages", len(messages)).Msg("stream start")

	writeProgress(w, "planning", "Analyzing...")

	// Ensure session exists
	if err := core.SessionManager.GetOrCreate(ctx, threadID); err != nil {
		logger.Error().Err(err).Str("thread_id", threadID).Msg("session create failed")
		writeProgress(w, "error", "Failed to create session")
		sse.WriteSSE(w, cb.Finish("stop"))
		sse.WriteDone(w)
		return
	}

	// Build the user message from the last message (ADK session stores history)
	lastMsg := messages[len(messages)-1]
	userContent := genai.NewContentFromText(lastMsg.Content, genai.RoleUser)

	streamEvents(ctx, w, core, threadID, userContent, cb, logger)
}

// StreamResumeRun handles the resume flow after HITL approval/denial.
// It sends the confirmation FunctionResponse back through the runner.
func StreamResumeRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, pending *PendingInterrupt, approved bool, logger zerolog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-resume-%s", uuid.New().String()[:12])
	cb := sse.NewChunkBuilder(requestID, core.AgentID, threadID)

	// Emit synthetic tool call delta so the frontend sees the tool_call
	// before the tool_result (required by Vercel AI SDK data stream protocol).
	argsJSON, _ := json.Marshal(pending.Details)
	sse.WriteSSE(w, cb.ToolCallDelta(0, pending.ToolCallID, pending.ToolName, string(argsJSON)))
	sse.WriteSSE(w, cb.Finish("tool_calls"))

	// Build the confirmation FunctionResponse per ADK's expected format
	funcResponse := &genai.FunctionResponse{
		Name: toolconfirmation.FunctionCallName, // "adk_request_confirmation"
		ID:   pending.ConfirmationCallID,
		Response: map[string]any{
			"confirmed": approved,
		},
	}

	confirmContent := &genai.Content{
		Role:  string(genai.RoleUser),
		Parts: []*genai.Part{{FunctionResponse: funcResponse}},
	}

	streamEvents(ctx, w, core, threadID, confirmContent, cb, logger)
}

// streamEvents is the shared event processing loop for both new turns and resumes.
// For workflow agents (Sequential/Parallel/Loop), multiple sub-agents emit events.
// We let the runner complete rather than closing on the first IsFinalResponse().
//
// Agent event routing:
//   - If core.OutputAgent is set, only that agent's text goes to choices[0].delta.content.
//     All other agents' text is routed to custom agent_event SSE events.
//   - If core.OutputAgent is empty (flat agent), all text goes to choices[0].delta.content.
//   - Agent transitions emit agent_progress events (agent_start/agent_done) with step tracking.
func streamEvents(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, content *genai.Content, cb *sse.ChunkBuilder, logger zerolog.Logger) {
	toolCallIdx := int64(0)
	hadPartialText := false
	step := 0
	lastAuthor := ""

	// Agent transition tracking.
	//
	// The challenge: ParallelAgent interleaves streaming tokens between sub-agents
	// (document_analyst, data_analyst), which would cause hundreds of agent_start/
	// agent_done pairs if we tracked naively. We solve this by tracking a set of
	// "parallel peers" — agents that interleave with each other.
	//
	// When a NEW agent appears that isn't a returning parallel peer, we close all
	// agents in the previous group and start a new group.
	activeGroup := make(map[string]bool) // agents in current parallel group
	allSeen := make(map[string]bool)     // all agents ever seen (for loop detection)

	streamLog := logger.With().Str("thread_id", threadID).Str("agent_id", core.AgentID).Logger()

	isOutputAgent := func(author string) bool {
		return core.OutputAgent == "" || author == core.OutputAgent
	}

	closeGroup := func() {
		for a := range activeGroup {
			writeAgentProgress(w, "agent_done", fmt.Sprintf("%s completed", a), a, step)
			streamLog.Debug().Str("sub_agent", a).Int("step", step).Msg("agent done")
			// Remove from allSeen so the same agents can re-form a group
			// in the next loop iteration (e.g. document_analyst on doc 2).
			// The triggering agent (the one that caused the close) stays in
			// allSeen since it's added after this function returns.
			delete(allSeen, a)
		}
		activeGroup = make(map[string]bool)
	}

	transitionAgent := func(author string) {
		if author == "" || author == lastAuthor {
			return
		}
		lastAuthor = author

		// Already in current group (parallel peer returning) — skip
		if activeGroup[author] {
			return
		}

		// Determine if we should close the current group before starting this agent.
		shouldClose := false
		if allSeen[author] {
			// Agent seen before (e.g. document_fetcher on doc 2) — new phase
			shouldClose = true
		} else if len(activeGroup) == 1 {
			// Single active agent → sequential step, close it
			shouldClose = true
		}
		// If activeGroup has multiple agents and this is a new agent, it's likely
		// another parallel peer joining — don't close.

		if shouldClose {
			closeGroup()
		}

		allSeen[author] = true
		activeGroup[author] = true
		step++
		writeAgentProgress(w, "agent_start", fmt.Sprintf("Running %s...", author), author, step)
		streamLog.Info().Str("sub_agent", author).Int("step", step).Msg("agent start")
	}

	streamLog.Info().Msg("starting agent run")

	for event, err := range core.Runner.Run(ctx, "default", threadID, content, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeSSE,
	}) {
		if err != nil {
			streamLog.Error().Err(err).Msg("runner error")
			writeProgress(w, "error", fmt.Sprintf("Error: %v", err))
			break
		}

		if event.Content == nil {
			continue
		}

		author := event.Author
		transitionAgent(author)

		// Partial event = streaming text token
		if event.Partial {
			for _, part := range event.Content.Parts {
				if part.Text != "" {
					if isOutputAgent(author) {
						sse.WriteSSE(w, cb.TextDelta(part.Text))
					} else {
						writeAgentEvent(w, author, "text_delta", part.Text, step)
					}
					hadPartialText = true
				}
			}
			continue
		}

		// Non-partial event — process parts
		for _, part := range event.Content.Parts {
			// Text from non-streaming sub-agents or code agents (no prior partials)
			if part.Text != "" && !hadPartialText {
				if isOutputAgent(author) {
					sse.WriteSSE(w, cb.TextDelta(part.Text))
				} else {
					writeAgentEvent(w, author, "text_delta", part.Text, step)
				}
			}

			// Emit text_done for intermediate agents when non-partial text arrives
			if part.Text != "" && !isOutputAgent(author) {
				writeAgentEvent(w, author, "text_done", "", step)
			}

			// Tool confirmation request (adk_request_confirmation)
			if fc := part.FunctionCall; fc != nil && fc.Name == toolconfirmation.FunctionCallName {
				originalCall, err := toolconfirmation.OriginalCallFrom(fc)
				if err != nil {
					logger.Error().Err(err).Msg("failed to extract original call from confirmation")
					continue
				}

				// Emit the tool call delta for the original tool so the UI sees it
				argsJSON, _ := json.Marshal(originalCall.Args)
				sse.WriteSSE(w, cb.ToolCallDelta(toolCallIdx, originalCall.ID, originalCall.Name, string(argsJSON)))
				sse.WriteSSE(w, cb.Finish("tool_calls"))
				toolCallIdx++

				// Extract hint from the confirmation args
				hint := ""
				if args, ok := fc.Args["hint"].(string); ok {
					hint = args
				}

				// Store the pending interrupt so the resume endpoint can find it
				core.Interrupts.Set(threadID, &PendingInterrupt{
					ConfirmationCallID: fc.ID,
					ToolCallID:         originalCall.ID,
					ToolName:           originalCall.Name,
					Prompt:             hint,
					Details:            originalCall.Args,
				})

				// Emit tool_interrupt SSE event for the frontend
				evt := types.ToolInterruptEvent{}
				evt.ToolInterrupt.ToolCallID = originalCall.ID
				evt.ToolInterrupt.ToolName = originalCall.Name
				evt.ToolInterrupt.Prompt = hint
				evt.ToolInterrupt.Details = originalCall.Args
				evt.ToolInterrupt.ThreadID = threadID
				sse.WriteSSE(w, evt)

				// Stream is paused — finish and return
				sse.WriteSSE(w, cb.Finish("stop"))
				sse.WriteDone(w)
				return
			}

			// Regular tool call (non-confirmation) — emit delta for UI
			if fc := part.FunctionCall; fc != nil {
				streamLog.Info().Str("sub_agent", author).Str("tool", fc.Name).Str("call_id", fc.ID).Msg("tool call")
				writeProgress(w, "executing", fmt.Sprintf("Running %s...", fc.Name))
				argsJSON, _ := json.Marshal(fc.Args)
				sse.WriteSSE(w, cb.ToolCallDelta(toolCallIdx, fc.ID, fc.Name, string(argsJSON)))
				sse.WriteSSE(w, cb.Finish("tool_calls"))
				toolCallIdx++
			}

			// Tool response (FunctionResponse) — emit tool_result for UI
			if fr := part.FunctionResponse; fr != nil {
				// Skip confirmation responses (internal ADK plumbing)
				if fr.Name == toolconfirmation.FunctionCallName {
					continue
				}
				streamLog.Debug().Str("sub_agent", author).Str("tool", fr.Name).Str("call_id", fr.ID).Msg("tool result")
				evt := types.ToolResultEvent{}
				evt.ToolResult.ToolCallID = fr.ID
				evt.ToolResult.ToolName = fr.Name
				evt.ToolResult.Result = fr.Response
				sse.WriteSSE(w, evt)
			}
		}

		// Reset partial tracking after each non-partial event
		hadPartialText = false
	}

	// Close remaining active agents
	closeGroup()

	streamLog.Info().Int("steps", step).Msg("agent run complete")

	// Stream closes when the runner completes (all workflow agents finished)
	sse.WriteSSE(w, cb.Finish("stop"))
	sse.WriteDone(w)
}

func writeProgress(w http.ResponseWriter, phase, message string) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
	sse.WriteSSE(w, evt)
}

func writeAgentProgress(w http.ResponseWriter, phase, message, agentName string, step int) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
	evt.AgentProgress.Agent = agentName
	evt.AgentProgress.Step = step
	sse.WriteSSE(w, evt)
}

func writeAgentEvent(w http.ResponseWriter, agentName, eventType, content string, step int) {
	evt := types.AgentEventEvent{}
	evt.AgentEvent.Agent = agentName
	evt.AgentEvent.Type = eventType
	evt.AgentEvent.Content = content
	evt.AgentEvent.Step = step
	sse.WriteSSE(w, evt)
}

func messagesToContents(messages []types.ChatMessage) []*genai.Content {
	var contents []*genai.Content
	for _, msg := range messages {
		var role string
		switch msg.Role {
		case "user":
			role = genai.RoleUser
		case "assistant":
			role = genai.RoleModel
		default:
			continue
		}
		contents = append(contents, &genai.Content{
			Role:  role,
			Parts: []*genai.Part{{Text: msg.Content}},
		})
	}
	return contents
}
