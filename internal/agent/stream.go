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
	cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName, threadID)

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
	cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName, threadID)

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
func streamEvents(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, content *genai.Content, cb *sse.ChunkBuilder, logger zerolog.Logger) {
	toolCallIdx := int64(0)

	for event, err := range core.Runner.Run(ctx, "default", threadID, content, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeSSE,
	}) {
		if err != nil {
			logger.Error().Err(err).Msg("runner error")
			writeProgress(w, "error", fmt.Sprintf("Error: %v", err))
			break
		}

		if event.Content == nil {
			continue
		}

		// Partial event = streaming text token
		if event.Partial {
			for _, part := range event.Content.Parts {
				if part.Text != "" {
					sse.WriteSSE(w, cb.TextDelta(part.Text))
				}
			}
			continue
		}

		// Non-partial event — process parts
		for _, part := range event.Content.Parts {
			// Text part
			if part.Text != "" {
				// Final text (already streamed via partials, but mark the turn)
				// Don't re-emit — partials already sent the tokens
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
				evt := types.ToolResultEvent{}
				evt.ToolResult.ToolCallID = fr.ID
				evt.ToolResult.ToolName = fr.Name
				evt.ToolResult.Result = fr.Response
				sse.WriteSSE(w, evt)
			}
		}

		// Check if this is the final response
		if event.IsFinalResponse() {
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}
	}

	// If we exit the loop without a final response, close the stream
	sse.WriteSSE(w, cb.Finish("stop"))
	sse.WriteDone(w)
}

func writeProgress(w http.ResponseWriter, phase, message string) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
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
