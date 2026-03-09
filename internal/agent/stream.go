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
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

type contextKeyType string

const ThreadIDKey contextKeyType = "agentic_thread_id"

// StreamAgentRun executes the agent and streams SSE events to the response writer.
func StreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, userMsg *genai.Content, logger zerolog.Logger) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName)

	// Inject threadID into context for tool handlers
	ctx = context.WithValue(ctx, ThreadIDKey, threadID)

	// Emit initial progress
	writeProgress(w, "planning", "Analyzing...")

	// Run the agent
	for event, err := range core.Runner.Run(ctx, "default", threadID, userMsg, adkagent.RunConfig{}) {
		if err != nil {
			logger.Error().Err(err).Msg("agent run error")
			writeProgress(w, "error", fmt.Sprintf("Error: %v", err))
			break
		}

		if event.Content == nil {
			if event.TurnComplete {
				sse.WriteSSE(w, cb.Finish("stop"))
				sse.WriteDone(w)
				return
			}
			continue
		}

		interrupted := processEventParts(w, event, cb, core, threadID, logger)
		if interrupted {
			// HITL interrupt detected — stop streaming
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		if event.TurnComplete {
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}
	}

	// If we get here without TurnComplete, still send done
	sse.WriteSSE(w, cb.Finish("stop"))
	sse.WriteDone(w)
}

// processEventParts processes all parts of an event and writes SSE output.
// Returns true if a HITL interrupt was detected (caller should stop streaming).
func processEventParts(w http.ResponseWriter, event *session.Event, cb *sse.ChunkBuilder, core *Core, threadID string, logger zerolog.Logger) bool {
	for i, part := range event.Content.Parts {
		// Text content
		if part.Text != "" {
			sse.WriteSSE(w, cb.TextDelta(part.Text))
		}

		// Function call (tool invocation by the agent)
		if part.FunctionCall != nil {
			fc := part.FunctionCall
			logger.Debug().Str("tool", fc.Name).Msg("tool call")

			// Emit progress
			writeProgress(w, "executing", fmt.Sprintf("Running %s...", fc.Name))

			// Marshal args
			argsJSON, _ := json.Marshal(fc.Args)

			// Emit tool call delta
			sse.WriteSSE(w, cb.ToolCallDelta(int64(i), fc.ID, fc.Name, string(argsJSON)))
			sse.WriteSSE(w, cb.Finish("tool_calls"))
		}

		// Function response (tool result)
		if part.FunctionResponse != nil {
			fr := part.FunctionResponse

			// Check for HITL marker
			if isHITLResponse(fr.Response) {
				logger.Info().Str("tool", fr.Name).Str("thread_id", threadID).Msg("HITL interrupt")

				pending := core.HITLStore.GetPending(threadID)
				if pending == nil {
					// Build from response data
					pending = &PendingConfirmation{
						ToolCallID: fr.ID,
						ToolName:   fr.Name,
					}
					if p, ok := fr.Response["prompt"].(string); ok {
						pending.Prompt = p
					}
					if d, ok := fr.Response["details"]; ok {
						pending.Details = d
					}
					core.HITLStore.SetPending(threadID, pending)
				}

				// Emit tool_interrupt
				evt := types.ToolInterruptEvent{}
				evt.ToolInterrupt.ToolCallID = pending.ToolCallID
				evt.ToolInterrupt.ToolName = pending.ToolName
				evt.ToolInterrupt.Prompt = pending.Prompt
				evt.ToolInterrupt.Details = pending.Details
				evt.ToolInterrupt.ThreadID = threadID
				sse.WriteSSE(w, evt)

				return true // signal interrupt
			}

			// Normal tool result
			evt := types.ToolResultEvent{}
			evt.ToolResult.ToolCallID = fr.ID
			evt.ToolResult.ToolName = fr.Name
			evt.ToolResult.Result = fr.Response
			sse.WriteSSE(w, evt)
		}
	}
	return false
}

// isHITLResponse checks if a function response indicates HITL is needed.
func isHITLResponse(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	v, ok := resp["requires_confirmation"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

func writeProgress(w http.ResponseWriter, phase, message string) {
	evt := types.AgentProgressEvent{}
	evt.AgentProgress.Phase = phase
	evt.AgentProgress.Message = message
	sse.WriteSSE(w, evt)
}
