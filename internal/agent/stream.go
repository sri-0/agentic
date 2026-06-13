package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agentic/internal/chat"
	"agentic/internal/hitl"
	"agentic/internal/sse"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

// StreamAgentRun uses the ADK runner with StreamingModeSSE to execute the
// agent loop. If saver is non-nil, user and assistant messages are persisted.
func StreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, messages []types.ChatMessage, saver *chat.MessageSaver, logger zerolog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	cb := sse.NewChunkBuilder(requestID, core.AgentID, threadID)

	logger.Info().Str("thread_id", threadID).Str("agent_id", core.AgentID).Int("messages", len(messages)).Msg("stream: request received")

	writeProgress(w, threadID, requestID, "planning", "Analyzing...")

	// Ensure session exists
	if err := core.SessionManager.GetOrCreate(ctx, threadID); err != nil {
		logger.Error().Err(err).Str("thread_id", threadID).Msg("stream: session create failed")
		writeProgress(w, threadID, requestID, "error", "Failed to create session")
		sse.WriteSSE(w, cb.Finish("stop"))
		sse.WriteDone(w)
		return
	}
	logger.Info().Str("thread_id", threadID).Msg("stream: session ready")

	// Build the user message from the last message (ADK session stores history)
	lastMsg := messages[len(messages)-1]
	userContent := genai.NewContentFromText(lastMsg.Content, genai.RoleUser)

	// Persist user message
	if saver != nil {
		saver.SaveUserMessage(ctx, threadID, lastMsg.Content)
	}

	streamEvents(ctx, w, core, threadID, requestID, userContent, cb, saver, logger)
}

// StreamResumeRun handles the resume flow after HITL approval/denial.
// It sends the confirmation FunctionResponse back through the runner.
func StreamResumeRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, pending *hitl.PendingInterrupt, approved bool, logger zerolog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-resume-%s", uuid.New().String()[:12])
	cb := sse.NewChunkBuilder(requestID, core.AgentID, threadID)

	action := "denied"
	if approved {
		action = "approved"
	}
	logger.Info().
		Str("thread_id", threadID).
		Str("agent_id", core.AgentID).
		Str("tool", pending.ToolName).
		Str("tool_call_id", pending.ToolCallID).
		Str("action", action).
		Msg("resume: continuing after HITL")

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

	streamEvents(ctx, w, core, threadID, requestID, confirmContent, cb, nil, logger)
}

// streamEvents is the shared event processing loop for both new turns and resumes.
func streamEvents(ctx context.Context, w http.ResponseWriter, core *Core, threadID, runID string, content *genai.Content, cb *sse.ChunkBuilder, saver *chat.MessageSaver, logger zerolog.Logger) {
	toolCallIdx := int64(0)
	hadPartialText := false
	var outputText string // accumulates output agent text for persistence
	step := 0
	lastAuthor := ""
	var lastUsage usageTotals // most recent token usage seen (final wins)

	// Task-list tracking for multi-agent runs. One task per sub-agent;
	// status transitions pending -> in_progress -> completed. A full snapshot
	// is re-emitted on every change. Single-agent runs skip this entirely.
	multiAgent := len(core.SubAgentNames) > 0
	taskStatus := make(map[string]string)
	if multiAgent {
		for _, name := range core.SubAgentNames {
			taskStatus[name] = "pending"
		}
	}
	emitTaskList := func() {
		if !multiAgent {
			return
		}
		writeTaskList(w, threadID, runID, core.SubAgentNames, taskStatus)
	}
	setTaskStatus := func(name, status string) {
		if !multiAgent {
			return
		}
		// Only track known sub-agents; ignore the root/orchestrator author.
		if _, known := taskStatus[name]; !known {
			return
		}
		if taskStatus[name] == status {
			return
		}
		taskStatus[name] = status
		emitTaskList()
	}

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
			writeAgentProgress(w, threadID, runID, "agent_done", fmt.Sprintf("%s completed", a), a, step)
			setTaskStatus(a, "completed")
			streamLog.Info().Str("sub_agent", a).Int("step", step).Msg("stream: agent done")
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
		writeAgentProgress(w, threadID, runID, "agent_start", fmt.Sprintf("Running %s...", author), author, step)
		setTaskStatus(author, "in_progress")
		streamLog.Info().Str("sub_agent", author).Int("step", step).Msg("stream: agent start")
	}

	startTime := time.Now()
	eventCount := 0
	toolCallCount := 0
	streamLog.Info().Msg("stream: runner started")
	writeAGUI(w, &types.AGUIEvent{Type: "RUN_STARTED", Timestamp: time.Now().UnixMilli(), ThreadID: threadID, RunID: runID})

	for event, err := range core.Runner.Run(ctx, "default", threadID, content, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeSSE,
	}) {
		if err != nil {
			streamLog.Error().Err(err).Dur("elapsed", time.Since(startTime)).Msg("stream: runner error")
			writeProgress(w, threadID, runID, "error", fmt.Sprintf("Error: %v", err))
			break
		}

		if event.Content == nil {
			continue
		}

		eventCount++
		author := event.Author
		transitionAgent(author)

		// Partial event = streaming text token
		if event.Partial {
			for _, part := range event.Content.Parts {
				if part.Text == "" {
					continue
				}
				// Thought parts carry the model's reasoning, not output text.
				if part.Thought {
					if isOutputAgent(author) {
						sse.WriteSSE(w, cb.ReasoningDelta(part.Text))
					} else {
						writeReasoning(w, threadID, runID, author, part.Text, step)
					}
					continue
				}
				if isOutputAgent(author) {
					sse.WriteSSE(w, cb.TextDelta(part.Text))
					outputText += part.Text
				} else {
					writeAgentEvent(w, threadID, runID, author, "text_delta", part.Text, step)
				}
				hadPartialText = true
			}
			continue
		}

		// Capture token usage as it arrives. The final (terminal) usage wins,
		// and we also track the last prompt token count as running context size.
		if event.UsageMetadata != nil {
			um := event.UsageMetadata
			if um.TotalTokenCount > 0 || um.PromptTokenCount > 0 {
				lastUsage = usageTotals{
					prompt:     int(um.PromptTokenCount),
					completion: int(um.CandidatesTokenCount),
					total:      int(um.TotalTokenCount),
				}
			}
		}

		// Non-partial event — process parts
		for _, part := range event.Content.Parts {
			// Thought parts from non-streaming agents → reasoning.
			if part.Text != "" && part.Thought {
				if isOutputAgent(author) {
					sse.WriteSSE(w, cb.ReasoningDelta(part.Text))
				} else {
					writeReasoning(w, threadID, runID, author, part.Text, step)
				}
				continue
			}

			// Text from non-streaming sub-agents or code agents (no prior partials)
			if part.Text != "" && !hadPartialText {
				if isOutputAgent(author) {
					sse.WriteSSE(w, cb.TextDelta(part.Text))
					outputText += part.Text
				} else {
					writeAgentEvent(w, threadID, runID, author, "text_delta", part.Text, step)
				}
			}

			// Emit text_done for intermediate agents when non-partial text arrives
			if part.Text != "" && !isOutputAgent(author) {
				writeAgentEvent(w, threadID, runID, author, "text_done", "", step)
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

				streamLog.Info().
					Str("tool", originalCall.Name).
					Str("tool_call_id", originalCall.ID).
					Str("hint", hint).
					Dur("elapsed", time.Since(startTime)).
					Msg("stream: HITL interrupt — awaiting confirmation")

				// Store the pending interrupt so the resume endpoint can find it
				if err := core.Interrupts.Set(threadID, &hitl.PendingInterrupt{
					AgentID:            core.AgentID,
					ConfirmationCallID: fc.ID,
					ToolCallID:         originalCall.ID,
					ToolName:           originalCall.Name,
					Prompt:             hint,
					Details:            originalCall.Args,
				}); err != nil {
					logger.Error().Err(err).Msg("failed to store HITL interrupt")
				}

				// Emit tool_interrupt SSE event for the frontend
				evt := types.ToolInterruptEvent{}
				evt.ToolInterrupt.ToolCallID = originalCall.ID
				evt.ToolInterrupt.ToolName = originalCall.Name
				evt.ToolInterrupt.Prompt = hint
				evt.ToolInterrupt.Details = originalCall.Args
				evt.ToolInterrupt.ThreadID = threadID
				evt.AGUI = &types.AGUIEvent{
					Type:         "CUSTOM",
					Timestamp:    time.Now().UnixMilli(),
					ThreadID:     threadID,
					RunID:        runID,
					Name:         "tool_interrupt",
					ToolCallID:   originalCall.ID,
					ToolCallName: originalCall.Name,
					Value: map[string]any{
						"prompt":  hint,
						"details": originalCall.Args,
					},
				}
				sse.WriteSSE(w, evt)

				// Stream is paused — finish and return
				sse.WriteSSE(w, cb.Finish("stop"))
				sse.WriteDone(w)
				return
			}

			// Regular tool call (non-confirmation) — emit delta for UI
			if fc := part.FunctionCall; fc != nil {
				toolCallCount++
				streamLog.Info().Str("sub_agent", author).Str("tool", fc.Name).Str("call_id", fc.ID).Int("tool_call_num", toolCallCount).Dur("elapsed", time.Since(startTime)).Msg("stream: tool call")
				writeProgress(w, threadID, runID, "executing", fmt.Sprintf("Running %s...", fc.Name))
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

				// emit_artifact tool → push an artifact CUSTOM event to the UI.
				// The tool returns the artifact payload in its response.
				if fr.Name == artifactToolName {
					writeArtifact(w, threadID, runID, fr.Response)
					// fall through to also emit a tool_result so the call/result
					// pairing stays intact for the frontend.
				}
				streamLog.Info().Str("sub_agent", author).Str("tool", fr.Name).Str("call_id", fr.ID).Dur("elapsed", time.Since(startTime)).Msg("stream: tool result")
				evt := types.ToolResultEvent{}
				evt.ToolResult.ToolCallID = fr.ID
				evt.ToolResult.ToolName = fr.Name
				evt.ToolResult.Result = fr.Response
				content, _ := json.Marshal(fr.Response)
				evt.AGUI = &types.AGUIEvent{
					Type:         "TOOL_CALL_RESULT",
					Timestamp:    time.Now().UnixMilli(),
					ThreadID:     threadID,
					RunID:        runID,
					MessageID:    fmt.Sprintf("tool-%s", fr.ID),
					ToolCallID:   fr.ID,
					ToolCallName: fr.Name,
					Content:      string(content),
				}
				sse.WriteSSE(w, evt)
			}
		}

		// Reset partial tracking after each non-partial event
		hadPartialText = false
	}

	// Close remaining active agents
	closeGroup()

	// Persist assistant message
	if saver != nil && outputText != "" {
		saver.SaveAssistantMessage(ctx, threadID, outputText, core.AgentID)
	}

	streamLog.Info().
		Int("steps", step).
		Int("events", eventCount).
		Int("tool_calls", toolCallCount).
		Int("output_chars", len(outputText)).
		Dur("elapsed", time.Since(startTime)).
		Msg("stream: agent run complete")

	// Emit the context/usage snapshot just before RUN_FINISHED.
	contextWindow := 0
	if core.Config != nil && core.Config.Models != nil && core.ModelID != "" {
		if m := core.Config.Models.FindModel(core.ModelID); m != nil {
			contextWindow = m.ContextLength
		}
	}
	writeContextUsage(w, threadID, runID, lastUsage, contextWindow)

	// Stream closes when the runner completes (all workflow agents finished)
	writeAGUI(w, &types.AGUIEvent{Type: "RUN_FINISHED", Timestamp: time.Now().UnixMilli(), ThreadID: threadID, RunID: runID})
	if lastUsage.total > 0 {
		sse.WriteSSE(w, cb.FinishWithUsage("stop", lastUsage.prompt, lastUsage.completion, lastUsage.total))
	} else {
		sse.WriteSSE(w, cb.Finish("stop"))
	}
	sse.WriteDone(w)
}

func writeProgress(w http.ResponseWriter, threadID, runID, phase, message string) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
	evt.AGUI = &types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  threadID,
		RunID:     runID,
		Name:      "agent_progress",
		Value: map[string]any{
			"phase":   phase,
			"message": message,
		},
	}
	sse.WriteSSE(w, evt)
}

func writeAgentProgress(w http.ResponseWriter, threadID, runID, phase, message, agentName string, step int) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
	evt.AgentProgress.Agent = agentName
	evt.AgentProgress.Step = step
	aguiType := "STEP_STARTED"
	if phase == "agent_done" {
		aguiType = "STEP_FINISHED"
	}
	evt.AGUI = &types.AGUIEvent{
		Type:      aguiType,
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  threadID,
		RunID:     runID,
		StepName:  agentName,
		RawEvent: map[string]any{
			"phase":   phase,
			"message": message,
			"step":    step,
		},
	}
	sse.WriteSSE(w, evt)
}

func writeAgentEvent(w http.ResponseWriter, threadID, runID, agentName, eventType, content string, step int) {
	evt := types.AgentEventEvent{}
	evt.AgentEvent.Agent = agentName
	evt.AgentEvent.Type = eventType
	evt.AgentEvent.Content = content
	evt.AgentEvent.Step = step
	aguiType := "CUSTOM"
	if eventType == "text_delta" {
		aguiType = "TEXT_MESSAGE_CONTENT"
	} else if eventType == "text_done" {
		aguiType = "TEXT_MESSAGE_END"
	}
	evt.AGUI = &types.AGUIEvent{
		Type:      aguiType,
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  threadID,
		RunID:     runID,
		MessageID: fmt.Sprintf("%s-%d", agentName, step),
		Delta:     content,
		RawEvent: map[string]any{
			"agent": agentName,
			"type":  eventType,
			"step":  step,
		},
	}
	sse.WriteSSE(w, evt)
}

func writeAGUI(w http.ResponseWriter, event *types.AGUIEvent) {
	sse.WriteSSE(w, map[string]any{"ag_ui": event})
}

// usageTotals holds token counts captured from the ADK run.
type usageTotals struct {
	prompt     int
	completion int
	total      int
}

// artifactToolName is the built-in tool agents call to push an artifact to the UI.
const artifactToolName = "emit_artifact"

// writeReasoning emits a sub-agent (non-output) reasoning delta. The output
// agent's reasoning is emitted via cb.ReasoningDelta instead.
func writeReasoning(w http.ResponseWriter, threadID, runID, agentName, content string, step int) {
	evt := types.AgentEventEvent{}
	evt.AgentEvent.Agent = agentName
	evt.AgentEvent.Type = "reasoning_delta"
	evt.AgentEvent.Content = content
	evt.AgentEvent.Step = step
	evt.AGUI = &types.AGUIEvent{
		Type:      "THINKING_TEXT_MESSAGE_CONTENT",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  threadID,
		RunID:     runID,
		MessageID: fmt.Sprintf("%s-%d", agentName, step),
		Delta:     content,
		RawEvent: map[string]any{
			"agent": agentName,
			"type":  "reasoning_delta",
			"step":  step,
		},
	}
	sse.WriteSSE(w, evt)
}

// writeContextUsage emits a CUSTOM "context_usage" event carrying token usage
// and a best-effort context breakdown. context_used is the running context
// size (prompt tokens of the final turn); context_window is the model's
// configured context_length (0 if unknown).
func writeContextUsage(w http.ResponseWriter, threadID, runID string, u usageTotals, contextWindow int) {
	// Best-effort breakdown. We cannot bucket prompt tokens precisely without
	// per-source accounting, so we attribute prompt tokens to History and
	// completion tokens to Completion, with System as a nominal sub-bucket.
	breakdown := []map[string]any{
		{"label": "System", "tokens": 0},
		{"label": "History", "tokens": u.prompt},
		{"label": "Completion", "tokens": u.completion},
	}
	evt := types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  threadID,
		RunID:     runID,
		Name:      "context_usage",
		Value: map[string]any{
			"prompt_tokens":     u.prompt,
			"completion_tokens": u.completion,
			"total_tokens":      u.total,
			"context_used":      u.prompt,
			"context_window":    contextWindow,
			"breakdown":         breakdown,
		},
	}
	sse.WriteSSE(w, map[string]any{"ag_ui": evt})
}

// writeTaskList emits a CUSTOM "task_list" snapshot: one task per sub-agent,
// in declaration order, with its current status.
func writeTaskList(w http.ResponseWriter, threadID, runID string, order []string, status map[string]string) {
	tasks := make([]map[string]any, 0, len(order))
	for _, name := range order {
		s := status[name]
		if s == "" {
			s = "pending"
		}
		tasks = append(tasks, map[string]any{
			"id":     name,
			"title":  name,
			"status": s,
		})
	}
	evt := types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  threadID,
		RunID:     runID,
		Name:      "task_list",
		Value:     map[string]any{"tasks": tasks},
	}
	sse.WriteSSE(w, map[string]any{"ag_ui": evt})
}

// writeArtifact emits a CUSTOM "artifact" event from an emit_artifact tool
// response. The response map carries id/title/kind/content/language.
func writeArtifact(w http.ResponseWriter, threadID, runID string, response map[string]any) {
	str := func(key string) string {
		if v, ok := response[key].(string); ok {
			return v
		}
		return ""
	}
	id := str("id")
	if id == "" {
		id = uuid.New().String()
	}
	value := map[string]any{
		"id":      id,
		"title":   str("title"),
		"kind":    str("kind"),
		"content": str("content"),
	}
	if lang := str("language"); lang != "" {
		value["language"] = lang
	}
	evt := types.AGUIEvent{
		Type:      "CUSTOM",
		Timestamp: time.Now().UnixMilli(),
		ThreadID:  threadID,
		RunID:     runID,
		Name:      "artifact",
		Value:     value,
	}
	sse.WriteSSE(w, map[string]any{"ag_ui": evt})
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
