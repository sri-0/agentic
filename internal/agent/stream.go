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
	"google.golang.org/genai"
)

type contextKeyType string

const ThreadIDKey contextKeyType = "agentic_thread_id"

const maxReActIterations = 10

// StreamAgentRun executes the agent in a ReAct loop, manually executing tools
// and feeding results back to the LLM until it produces a final text response.
func StreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, userMsg *genai.Content, logger zerolog.Logger) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName, threadID)

	// Inject threadID into context for tool handlers
	ctx = context.WithValue(ctx, ThreadIDKey, threadID)

	// Emit initial progress
	writeProgress(w, "planning", "Analyzing...")

	msg := userMsg

	for iteration := 0; iteration < maxReActIterations; iteration++ {
		// Collect function calls from this runner iteration
		var functionCalls []collectedCall

		for event, err := range core.Runner.Run(ctx, "default", threadID, msg, adkagent.RunConfig{}) {
			if err != nil {
				logger.Error().Err(err).Msg("agent run error")
				writeProgress(w, "error", fmt.Sprintf("Error: %v", err))
				sse.WriteSSE(w, cb.Finish("stop"))
				sse.WriteDone(w)
				return
			}

			if event.Content == nil {
				continue
			}

			toolCallIdx := 0
			for _, part := range event.Content.Parts {
				if part.Text != "" {
					sse.WriteSSE(w, cb.TextDelta(part.Text))
				}

				if part.FunctionCall != nil {
					fc := part.FunctionCall
					logger.Debug().Str("tool", fc.Name).Msg("tool call")

					writeProgress(w, "executing", fmt.Sprintf("Running %s...", fc.Name))

					argsJSON, _ := json.Marshal(fc.Args)
					sse.WriteSSE(w, cb.ToolCallDelta(int64(toolCallIdx), fc.ID, fc.Name, string(argsJSON)))
					sse.WriteSSE(w, cb.Finish("tool_calls"))

					functionCalls = append(functionCalls, collectedCall{
						ID:   fc.ID,
						Name: fc.Name,
						Args: fc.Args,
					})
					toolCallIdx++
				}
			}

			if event.TurnComplete {
				break
			}
		}

		// If no function calls, the agent produced a final text response
		if len(functionCalls) == 0 {
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		// Execute tools and build response content
		var responseParts []*genai.Part
		hitlInterrupted := false

		for _, fc := range functionCalls {
			result, err := core.ToolCaller.Call(fc.Name, fc.Args, threadID, fc.ID)
			if err != nil {
				result = map[string]any{"error": err.Error()}
			}

			// Check for HITL
			if isHITLResponse(result) {
				logger.Info().Str("tool", fc.Name).Str("thread_id", threadID).Msg("HITL interrupt")

				pending := core.HITLStore.GetPending(threadID)
				if pending == nil {
					pending = &PendingConfirmation{
						ToolCallID: fc.ID,
						ToolName:   fc.Name,
					}
					if p, ok := result["prompt"].(string); ok {
						pending.Prompt = p
					}
					if d, ok := result["details"]; ok {
						pending.Details = d
					}
					core.HITLStore.SetPending(threadID, pending)
				}

				evt := types.ToolInterruptEvent{}
				evt.ToolInterrupt.ToolCallID = pending.ToolCallID
				evt.ToolInterrupt.ToolName = pending.ToolName
				evt.ToolInterrupt.Prompt = pending.Prompt
				evt.ToolInterrupt.Details = pending.Details
				evt.ToolInterrupt.ThreadID = threadID
				sse.WriteSSE(w, evt)

				hitlInterrupted = true
				break
			}

			// Emit tool result event
			evt := types.ToolResultEvent{}
			evt.ToolResult.ToolCallID = fc.ID
			evt.ToolResult.ToolName = fc.Name
			evt.ToolResult.Result = result
			sse.WriteSSE(w, evt)

			responseParts = append(responseParts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name:     fc.Name,
					ID:       fc.ID,
					Response: result,
				},
			})
		}

		if hitlInterrupted {
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		// Build next message with tool results and feed back to runner
		msg = &genai.Content{
			Parts: responseParts,
			Role:  "user",
		}
	}

	// Max iterations reached
	logger.Warn().Int("max", maxReActIterations).Msg("ReAct loop reached max iterations")
	sse.WriteSSE(w, cb.Finish("stop"))
	sse.WriteDone(w)
}

type collectedCall struct {
	ID   string
	Name string
	Args map[string]any
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
