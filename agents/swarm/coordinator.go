package swarm

import (
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"github.com/rs/zerolog"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// checkTasksRun inspects the coordinator's output (stored in swarm:task_board).
//
// The coordinator's OutputKey is "swarm:task_board". Its text output is one of:
//   - A JSON array of tasks → dispatch should process pending tasks
//   - Anything else → treated as the final synthesis, triggers escalation
//
// This code agent detects which case it is and acts accordingly.
func checkTasksRun(
	maxIterations int,
	logger zerolog.Logger,
) func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			iteration := stateInt(ctx, KeyIteration) + 1
			boardRaw := stateString(ctx, KeyTaskBoard)

			logger.Info().Int("iteration", iteration).Int("max", maxIterations).Str("board_len", fmt.Sprintf("%d", len(boardRaw))).Msg("check_tasks")

			// Check if coordinator already set a synthesis directly
			if synthesis := stateString(ctx, KeySynthesis); synthesis != "" {
				logger.Info().Msg("synthesis found in state, escalating")
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "check_tasks"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText(synthesis, genai.RoleModel),
				}
				evt.Actions.Escalate = true
				yield(evt, nil)
				return
			}

			// Try to parse coordinator output as task board JSON
			// Strip markdown code fences if present
			cleaned := strings.TrimSpace(boardRaw)
			if strings.HasPrefix(cleaned, "```") {
				// Remove ```json ... ``` wrapping
				lines := strings.Split(cleaned, "\n")
				if len(lines) > 2 {
					cleaned = strings.Join(lines[1:len(lines)-1], "\n")
				}
			}

			var tasks []Task
			isTaskBoard := json.Unmarshal([]byte(cleaned), &tasks) == nil && len(tasks) > 0

			if !isTaskBoard {
				// Coordinator output is not a task list — it's the final synthesis
				logger.Info().Msg("coordinator produced synthesis (non-JSON output)")
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "check_tasks"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText(boardRaw, genai.RoleModel),
				}
				evt.Actions.StateDelta = map[string]any{
					KeySynthesis: boardRaw,
					KeyIteration: iteration,
				}
				evt.Actions.Escalate = true
				yield(evt, nil)
				return
			}

			// Valid task board — normalize it back to clean JSON
			normalizedBoard, _ := json.Marshal(tasks)

			// Check if all tasks are done (no pending)
			hasPending := false
			for _, t := range tasks {
				if t.Status == "pending" {
					hasPending = true
					break
				}
			}

			if !hasPending {
				// All done, but coordinator didn't synthesize yet — let it loop
				logger.Info().Int("tasks", len(tasks)).Msg("no pending tasks, coordinator will review next iteration")
			}

			// Check max iterations
			if maxIterations > 0 && iteration >= maxIterations {
				logger.Warn().Int("iteration", iteration).Msg("max iterations reached, forcing completion")
				summary := buildResultsSummary(string(normalizedBoard))
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "check_tasks"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText(summary, genai.RoleModel),
				}
				evt.Actions.StateDelta = map[string]any{
					KeySynthesis:  summary,
					KeyIteration:  iteration,
					KeyTaskBoard: string(normalizedBoard),
				}
				evt.Actions.Escalate = true
				yield(evt, nil)
				return
			}

			// Update state and continue loop
			evt := session.NewEvent(ctx.InvocationID())
			evt.Author = "check_tasks"
			evt.LLMResponse = model.LLMResponse{
				Content: genai.NewContentFromText(
					fmt.Sprintf("Iteration %d: %d tasks on board. Dispatching workers.", iteration, len(tasks)),
					genai.RoleModel,
				),
			}
			evt.Actions.StateDelta = map[string]any{
				KeyIteration: iteration,
				KeyTaskBoard: string(normalizedBoard),
			}
			yield(evt, nil)
		}
	}
}

// buildResultsSummary creates a text summary of all completed tasks.
func buildResultsSummary(rawBoard string) string {
	if rawBoard == "" {
		return "No tasks completed."
	}

	var tasks []Task
	if err := json.Unmarshal([]byte(rawBoard), &tasks); err != nil {
		return "Failed to parse task board."
	}

	var sb strings.Builder
	for _, t := range tasks {
		if t.Status == "done" && t.Result != "" {
			fmt.Fprintf(&sb, "## %s (worker: %s)\n%s\n\n", t.ID, t.Worker, t.Result)
		}
	}

	if sb.Len() == 0 {
		return "No tasks completed successfully."
	}
	return sb.String()
}

// buildWorkerManifest creates a description of available workers for the coordinator prompt.
func buildWorkerManifest(workers []workerInfo) string {
	var sb strings.Builder
	sb.WriteString("<available_workers>\n")
	for _, w := range workers {
		fmt.Fprintf(&sb, "- %s: %s", w.name, w.description)
		if len(w.tools) > 0 {
			fmt.Fprintf(&sb, " (tools: %s)", strings.Join(w.tools, ", "))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("</available_workers>")
	return sb.String()
}

type workerInfo struct {
	name        string
	description string
	tools       []string
}
