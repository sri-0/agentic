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
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

const maxReActIterations = 10

// StreamAgentRun executes the agent in a ReAct loop, calling the LLM directly
// with streaming enabled, manually executing tools, and feeding results back
// until the LLM produces a final text response.
func StreamAgentRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, messages []types.ChatMessage, logger zerolog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName, threadID)

	writeProgress(w, "planning", "Analyzing...")

	// Build conversation contents from incoming messages
	contents := messagesToContents(messages)

	// Append any stored conversation context (for resume continuations)
	stored := core.Conversations.Get(threadID)
	if len(stored) > 0 {
		contents = append(stored, contents...)
	}

	for iteration := 0; iteration < maxReActIterations; iteration++ {
		req := &model.LLMRequest{
			Model:    core.Model.Name(),
			Contents: contents,
			Config: &genai.GenerateContentConfig{
				SystemInstruction: genai.NewContentFromText(core.SystemInstruction, genai.RoleUser),
				Tools:             core.ToolDecls,
			},
		}

		var functionCalls []collectedCall
		var fullText string

		for resp, err := range core.Model.GenerateContent(ctx, req, true) {
			if err != nil {
				logger.Error().Err(err).Msg("model error")
				writeProgress(w, "error", fmt.Sprintf("Error: %v", err))
				sse.WriteSSE(w, cb.Finish("stop"))
				sse.WriteDone(w)
				return
			}

			if resp.Partial {
				// Streaming text token — emit immediately
				if resp.Content != nil {
					for _, part := range resp.Content.Parts {
						if part.Text != "" {
							sse.WriteSSE(w, cb.TextDelta(part.Text))
						}
					}
				}
				continue
			}

			// Final (non-partial) response — collect text and tool calls
			if resp.Content == nil {
				continue
			}

			toolCallIdx := 0
			for _, part := range resp.Content.Parts {
				if part.Text != "" {
					fullText += part.Text
					// Only emit if we didn't already stream it via partials
					// (the model yields partial tokens then a final aggregated response)
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
		}

		if len(functionCalls) == 0 {
			// No tool calls — final text response. Save and finish.
			if fullText != "" {
				contents = append(contents, &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{{Text: fullText}},
				})
			}
			core.Conversations.Append(threadID, contents...)
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		// Add the model's tool-call response to conversation
		var modelParts []*genai.Part
		for _, fc := range functionCalls {
			modelParts = append(modelParts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   fc.ID,
					Name: fc.Name,
					Args: fc.Args,
				},
			})
		}
		contents = append(contents, &genai.Content{
			Role:  genai.RoleModel,
			Parts: modelParts,
		})

		// Execute tools and build function responses
		var responseParts []*genai.Part
		hitlInterrupted := false

		for _, fc := range functionCalls {
			result, err := core.ToolCaller.Call(fc.Name, fc.Args, threadID, fc.ID)
			if err != nil {
				result = map[string]any{"error": err.Error()}
			}

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
			// Save conversation up to the tool call so resume can continue
			core.Conversations.Append(threadID, contents...)
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		// Add tool responses to conversation and loop back to the LLM
		contents = append(contents, &genai.Content{
			Parts: responseParts,
			Role:  "user",
		})
	}

	logger.Warn().Int("max", maxReActIterations).Msg("ReAct loop reached max iterations")
	core.Conversations.Append(threadID, contents...)
	sse.WriteSSE(w, cb.Finish("stop"))
	sse.WriteDone(w)
}

// StreamResumeRun handles the resume flow after HITL approval.
// It takes the tool result and continues the agent loop.
func StreamResumeRun(ctx context.Context, w http.ResponseWriter, core *Core, threadID string, toolCallID, toolName string, toolArgs map[string]any, toolResult map[string]any, logger zerolog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	requestID := fmt.Sprintf("chatcmpl-resume-%s", uuid.New().String()[:12])
	cb := sse.NewChunkBuilder(requestID, core.Config.AgentModelName, threadID)

	// Emit synthetic tool-call + result so the UI shows the flow
	argsJSON, _ := json.Marshal(toolArgs)
	sse.WriteSSE(w, cb.ToolCallDelta(0, toolCallID, toolName, string(argsJSON)))
	sse.WriteSSE(w, cb.Finish("tool_calls"))

	evt := types.ToolResultEvent{}
	evt.ToolResult.ToolCallID = toolCallID
	evt.ToolResult.ToolName = toolName
	evt.ToolResult.Result = toolResult
	sse.WriteSSE(w, evt)

	// Restore saved conversation and append the tool response
	contents := core.Conversations.Get(threadID)
	core.Conversations.Clear(threadID)

	contents = append(contents, &genai.Content{
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     toolName,
				ID:       toolCallID,
				Response: toolResult,
			},
		}},
		Role: "user",
	})

	// Continue the ReAct loop from here (reuse the main streaming logic)
	for iteration := 0; iteration < maxReActIterations; iteration++ {
		req := &model.LLMRequest{
			Model:    core.Model.Name(),
			Contents: contents,
			Config: &genai.GenerateContentConfig{
				SystemInstruction: genai.NewContentFromText(core.SystemInstruction, genai.RoleUser),
				Tools:             core.ToolDecls,
			},
		}

		var functionCalls []collectedCall
		var fullText string

		for resp, err := range core.Model.GenerateContent(ctx, req, true) {
			if err != nil {
				logger.Error().Err(err).Msg("model error during resume")
				sse.WriteSSE(w, cb.Finish("stop"))
				sse.WriteDone(w)
				return
			}

			if resp.Partial {
				if resp.Content != nil {
					for _, part := range resp.Content.Parts {
						if part.Text != "" {
							sse.WriteSSE(w, cb.TextDelta(part.Text))
						}
					}
				}
				continue
			}

			if resp.Content == nil {
				continue
			}

			toolCallIdx := 0
			for _, part := range resp.Content.Parts {
				if part.Text != "" {
					fullText += part.Text
				}
				if part.FunctionCall != nil {
					fc := part.FunctionCall
					writeProgress(w, "executing", fmt.Sprintf("Running %s...", fc.Name))
					argsJSON, _ := json.Marshal(fc.Args)
					sse.WriteSSE(w, cb.ToolCallDelta(int64(toolCallIdx), fc.ID, fc.Name, string(argsJSON)))
					sse.WriteSSE(w, cb.Finish("tool_calls"))
					functionCalls = append(functionCalls, collectedCall{ID: fc.ID, Name: fc.Name, Args: fc.Args})
					toolCallIdx++
				}
			}
		}

		if len(functionCalls) == 0 {
			if fullText != "" {
				contents = append(contents, &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{{Text: fullText}},
				})
			}
			core.Conversations.Append(threadID, contents...)
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		var modelParts []*genai.Part
		for _, fc := range functionCalls {
			modelParts = append(modelParts, &genai.Part{
				FunctionCall: &genai.FunctionCall{ID: fc.ID, Name: fc.Name, Args: fc.Args},
			})
		}
		contents = append(contents, &genai.Content{Role: genai.RoleModel, Parts: modelParts})

		var responseParts []*genai.Part
		hitlInterrupted := false

		for _, fc := range functionCalls {
			result, err := core.ToolCaller.Call(fc.Name, fc.Args, threadID, fc.ID)
			if err != nil {
				result = map[string]any{"error": err.Error()}
			}

			if isHITLResponse(result) {
				logger.Info().Str("tool", fc.Name).Msg("HITL interrupt during resume")
				pending := core.HITLStore.GetPending(threadID)
				if pending == nil {
					pending = &PendingConfirmation{ToolCallID: fc.ID, ToolName: fc.Name}
					if p, ok := result["prompt"].(string); ok {
						pending.Prompt = p
					}
					if d, ok := result["details"]; ok {
						pending.Details = d
					}
					core.HITLStore.SetPending(threadID, pending)
				}

				interruptEvt := types.ToolInterruptEvent{}
				interruptEvt.ToolInterrupt.ToolCallID = pending.ToolCallID
				interruptEvt.ToolInterrupt.ToolName = pending.ToolName
				interruptEvt.ToolInterrupt.Prompt = pending.Prompt
				interruptEvt.ToolInterrupt.Details = pending.Details
				interruptEvt.ToolInterrupt.ThreadID = threadID
				sse.WriteSSE(w, interruptEvt)

				hitlInterrupted = true
				break
			}

			toolEvt := types.ToolResultEvent{}
			toolEvt.ToolResult.ToolCallID = fc.ID
			toolEvt.ToolResult.ToolName = fc.Name
			toolEvt.ToolResult.Result = result
			sse.WriteSSE(w, toolEvt)

			responseParts = append(responseParts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{Name: fc.Name, ID: fc.ID, Response: result},
			})
		}

		if hitlInterrupted {
			core.Conversations.Append(threadID, contents...)
			sse.WriteSSE(w, cb.Finish("stop"))
			sse.WriteDone(w)
			return
		}

		contents = append(contents, &genai.Content{Parts: responseParts, Role: "user"})
	}

	core.Conversations.Append(threadID, contents...)
	sse.WriteSSE(w, cb.Finish("stop"))
	sse.WriteDone(w)
}

type collectedCall struct {
	ID   string
	Name string
	Args map[string]any
}

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
