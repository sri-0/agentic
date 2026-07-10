package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agentic/internal/chat"
	"agentic/internal/hitl"
	"agentic/internal/stream"
	"agentic/internal/stream/aisdk"
	"agentic/internal/stream/openai"
	"agentic/internal/tasks"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

// artifactToolName is the built-in tool agents call to push an artifact to the UI.
const artifactToolName = "emit_artifact"

// todoToolName is the built-in tool agents call to surface a structured todo
// list (task_list snapshot) to the UI.
const todoToolName = "todowrite"

// usageTotals holds token counts captured from the ADK run.
type usageTotals struct {
	prompt     int
	completion int
	total      int
}

// setStreamHeaders writes the SSE response headers (and the AI-SDK marker when
// the aisdk format is selected).
func setStreamHeaders(w http.ResponseWriter, format stream.Format) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if format == stream.FormatAISDK {
		w.Header().Set("x-vercel-ai-ui-message-stream", "v1")
	}
}

// newEncoder builds the wire-format encoder for the request. The OpenAI encoder
// keeps the agent id in the chunk `model` field (legacy behavior); the AI-SDK
// encoder stamps model + agent id onto message-metadata.
func newEncoder(format stream.Format, sink stream.Sink, requestID string, core *Core, threadID string) stream.Encoder {
	if format == stream.FormatAISDK {
		return aisdk.New(sink, core.ModelID, core.AgentID)
	}
	return openai.New(sink, requestID, core.AgentID, threadID)
}

// StreamAgentRun runs the agent loop with the default (OpenAI) wire format.
func StreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, messages []types.ChatMessage, saver *chat.MessageSaver, logger zerolog.Logger) {
	StreamAgentRunFormat(ctx, w, stream.FormatOpenAI, core, threadID, messages, saver, logger)
}

// StreamAgentRunFormat runs the agent loop, emitting the chosen wire format.
func StreamAgentRunFormat(ctx context.Context, w http.ResponseWriter, format stream.Format, core *Core, threadID string, messages []types.ChatMessage, saver *chat.MessageSaver, logger zerolog.Logger) {
	setStreamHeaders(w, format)

	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	enc := newEncoder(format, stream.NewSSESink(w), requestID, core, threadID)

	logger.Info().Str("thread_id", threadID).Str("agent_id", core.AgentID).Str("format", string(format)).Int("messages", len(messages)).Msg("stream: request received")

	enc.RunStarted()
	enc.Metadata(core.ModelID, core.AgentID, 0)
	enc.Progress("planning", "Analyzing...")

	// Ensure session exists
	if err := core.SessionManager.GetOrCreate(ctx, threadID); err != nil {
		logger.Error().Err(err).Str("thread_id", threadID).Msg("stream: session create failed")
		enc.Progress("error", "Failed to create session")
		enc.RunFinished(stream.Usage{})
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

	streamEvents(ctx, enc, core, threadID, requestID, userContent, saver, logger)
}

// StreamResumeRun handles the resume flow after HITL approval/denial (OpenAI format).
func StreamResumeRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, pending *hitl.PendingInterrupt, approved bool, logger zerolog.Logger) {
	StreamResumeRunFormat(ctx, w, stream.FormatOpenAI, core, threadID, pending, approved, logger)
}

// StreamResumeRunFormat handles the resume flow, emitting the chosen wire format.
// It sends the confirmation FunctionResponse back through the runner.
func StreamResumeRunFormat(ctx context.Context, w http.ResponseWriter, format stream.Format, core *Core, threadID string, pending *hitl.PendingInterrupt, approved bool, logger zerolog.Logger) {
	setStreamHeaders(w, format)

	requestID := fmt.Sprintf("chatcmpl-resume-%s", uuid.New().String()[:12])
	enc := newEncoder(format, stream.NewSSESink(w), requestID, core, threadID)

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

	enc.RunStarted()
	enc.Metadata(core.ModelID, core.AgentID, 0)

	// Re-surface the tool call so the frontend sees the tool_call before its
	// result (required by the stream protocol).
	argsJSON, _ := json.Marshal(pending.Details)
	enc.ToolCall(0, pending.ToolCallID, pending.ToolName, string(argsJSON))

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

	streamEvents(ctx, enc, core, threadID, requestID, confirmContent, nil, logger)
}

// streamEvents is the shared event processing loop for both new turns and
// resumes. It interprets adk-go events and drives the wire-format Encoder; it is
// format-agnostic (the encoder owns the byte format). RunStarted / initial
// metadata are emitted by the callers, so the AI-SDK stream opens with the
// required `start` chunk even on the resume path.
// streamEvents processes the run loop and returns the terminal outcome. A nil
// error means the run completed normally (done); a non-nil error is a runner
// failure that callers map to run-status{error} (H5) rather than silently
// reporting success. HITL interrupts are signalled via the encoder's
// Interrupted() (awaiting-input), independent of this error.
func streamEvents(ctx context.Context, enc stream.Encoder, core *Core, threadID, runID string, content *genai.Content, saver *chat.MessageSaver, logger zerolog.Logger) error {
	var runErr error
	toolCallIdx := int64(0)
	hadPartialText := false
	var outputText string // accumulates output agent text for persistence
	step := 0
	lastAuthor := ""
	lastBoardSig := ""              // last emitted task-board signature (de-dup)
	taskSeen := map[string]string{} // per-task high-water status (monotonic board)
	var lastUsage usageTotals       // most recent token usage seen (final wins)

	// Agent transition tracking.
	//
	// The challenge: ParallelAgent interleaves streaming tokens between sub-agents
	// (document_analyst, data_analyst), which would cause hundreds of agent_start/
	// agent_done pairs if we tracked naively. We solve this by tracking a set of
	// "parallel peers" — agents that interleave with each other.
	//
	// When a NEW agent appears that isn't a returning parallel peer, we close all
	// agents in the previous group and start a new group.
	activeGroup := make(map[string]bool)     // agents in current parallel group
	allSeen := make(map[string]bool)         // all agents ever seen (for loop detection)
	agentStart := make(map[string]time.Time) // per-agent start time (for duration)

	streamLog := logger.With().Str("thread_id", threadID).Str("agent_id", core.AgentID).Logger()

	// Worker instances are re-authored "worker#taskID" for unique per-task
	// identity; the output-agent check compares the base worker name.
	baseAgent := func(author string) string {
		if i := strings.IndexByte(author, '#'); i >= 0 {
			return author[:i]
		}
		return author
	}
	isOutputAgent := func(author string) bool {
		return core.OutputAgent == "" || baseAgent(author) == core.OutputAgent
	}

	closeGroup := func() {
		for a := range activeGroup {
			var durMs int64
			if t, ok := agentStart[a]; ok {
				durMs = time.Since(t).Milliseconds()
			}
			enc.AgentDone(a, step, durMs)
			streamLog.Info().Str("sub_agent", a).Int("step", step).Int64("duration_ms", durMs).Msg("stream: agent done")
			// Remove from allSeen so the same agents can re-form a group
			// in the next loop iteration (e.g. document_analyst on doc 2).
			delete(allSeen, a)
			delete(agentStart, a)
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
			shouldClose = true
		} else if len(activeGroup) == 1 {
			shouldClose = true
		}

		if shouldClose {
			closeGroup()
		}

		allSeen[author] = true
		activeGroup[author] = true
		agentStart[author] = time.Now()
		step++
		enc.AgentStart(author, step)
		streamLog.Info().Str("sub_agent", author).Int("step", step).Msg("stream: agent start")
	}

	startTime := time.Now()
	eventCount := 0
	toolCallCount := 0
	streamLog.Info().Msg("stream: runner started")

	for event, err := range core.Runner.Run(ctx, "default", threadID, content, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeSSE,
	}) {
		if err != nil {
			streamLog.Error().Err(err).Dur("elapsed", time.Since(startTime)).Msg("stream: runner error")
			if lastAuthor != "" {
				enc.AgentProgress("error", fmt.Sprintf("Error: %v", err), lastAuthor, step)
			} else {
				enc.Progress("error", fmt.Sprintf("Error: %v", err))
			}
			runErr = err
			break
		}

		// Task board updates can ride on content-less state-delta events, so
		// detect them BEFORE the Content==nil guard. De-dup by signature since
		// the coordinator re-writes the whole board each loop iteration.
		if board, ok := tasks.BoardFromStateDelta(event.Actions.StateDelta); ok {
			tasks.Clamp(taskSeen, board) // never regress a settled task in the UI
			if sig := tasks.Signature(board); sig != lastBoardSig {
				enc.TaskList(board)
				lastBoardSig = sig
			}
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
						enc.Reasoning(part.Text)
					} else {
						enc.AgentReasoning(author, step, part.Text)
					}
					continue
				}
				if isOutputAgent(author) {
					enc.Text(part.Text)
					outputText += part.Text
				} else {
					enc.AgentText(author, step, part.Text)
				}
				hadPartialText = true
			}
			continue
		}

		// Capture token usage as it arrives. The final (terminal) usage wins.
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
					enc.Reasoning(part.Text)
				} else {
					enc.AgentReasoning(author, step, part.Text)
				}
				continue
			}

			// Text from non-streaming sub-agents or code agents (no prior partials)
			if part.Text != "" && !hadPartialText {
				if isOutputAgent(author) {
					enc.Text(part.Text)
					outputText += part.Text
				} else {
					enc.AgentText(author, step, part.Text)
				}
			}

			// Emit text_done for intermediate agents when non-partial text arrives
			if part.Text != "" && !isOutputAgent(author) {
				enc.AgentTextDone(author, step)
			}

			// Tool confirmation request (adk_request_confirmation)
			if fc := part.FunctionCall; fc != nil && fc.Name == toolconfirmation.FunctionCallName {
				originalCall, err := toolconfirmation.OriginalCallFrom(fc)
				if err != nil {
					logger.Error().Err(err).Msg("failed to extract original call from confirmation")
					continue
				}

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

				// Emit the interrupt (the encoder owns the full pause sequence)
				// and return — the stream is paused awaiting resume.
				enc.ToolInterrupt(stream.Interrupt{
					ToolCallID: originalCall.ID,
					ToolName:   originalCall.Name,
					Prompt:     hint,
					Details:    originalCall.Args,
					ThreadID:   threadID,
				})
				return nil
			}

			// Regular tool call (non-confirmation) — emit delta for UI
			if fc := part.FunctionCall; fc != nil {
				toolCallCount++
				streamLog.Info().Str("sub_agent", author).Str("tool", fc.Name).Str("call_id", fc.ID).Int("tool_call_num", toolCallCount).Dur("elapsed", time.Since(startTime)).Msg("stream: tool call")
				argsJSON, _ := json.Marshal(fc.Args)
				if isOutputAgent(author) {
					// Main-thread tool call → output channel.
					enc.ToolCall(toolCallIdx, fc.ID, fc.Name, string(argsJSON))
					toolCallIdx++
				} else {
					// Sub-agent tool call → attributed, plus an "executing"
					// progress so it lands in the sub-agent's card.
					enc.AgentToolCall(author, step, fc.Name, fc.ID, string(argsJSON))
					enc.AgentProgress("executing", fmt.Sprintf("Running %s...", fc.Name), author, step)
				}
			}

			// Tool response (FunctionResponse) — emit tool_result for UI
			if fr := part.FunctionResponse; fr != nil {
				// Skip confirmation responses (internal ADK plumbing)
				if fr.Name == toolconfirmation.FunctionCallName {
					continue
				}

				// emit_artifact tool → push an artifact to the UI.
				if fr.Name == artifactToolName {
					enc.Artifact(fr.Response)
				}
				// todowrite tool → push a task_list snapshot.
				if fr.Name == todoToolName {
					enc.TaskList(tasks.MapTodos(fr.Response))
				}
				streamLog.Info().Str("sub_agent", author).Str("tool", fr.Name).Str("call_id", fr.ID).Dur("elapsed", time.Since(startTime)).Msg("stream: tool result")
				if isOutputAgent(author) {
					enc.ToolResult(fr.ID, fr.Name, fr.Response)
				} else {
					contentJSON, _ := json.Marshal(fr.Response)
					enc.AgentToolResult(author, step, fr.Name, fr.ID, string(contentJSON))
				}
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

	// Emit the context/usage snapshot just before the run closes.
	contextWindow := 0
	if core.Config != nil && core.Config.Models != nil && core.ModelID != "" {
		if m := core.Config.Models.FindModel(core.ModelID); m != nil {
			contextWindow = m.ContextLength
		}
	}
	u := stream.Usage{
		Prompt:        lastUsage.prompt,
		Completion:    lastUsage.completion,
		Total:         lastUsage.total,
		ContextWindow: contextWindow,
	}
	enc.Usage(u, usageBreakdown(lastUsage))

	// Final metadata carries the elapsed time, then close the run.
	enc.Metadata(core.ModelID, core.AgentID, time.Since(startTime).Milliseconds())
	enc.RunFinished(u)
	return runErr
}

// usageBreakdown attributes prompt tokens to History and completion tokens to
// Completion, with System as a nominal sub-bucket (best-effort, no per-source
// accounting available).
func usageBreakdown(u usageTotals) []stream.Bucket {
	return []stream.Bucket{
		{Label: "System", Tokens: 0},
		{Label: "History", Tokens: u.prompt},
		{Label: "Completion", Tokens: u.completion},
	}
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
