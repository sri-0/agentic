package coordinator

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

// checkTasksRun inspects the coordinator's output.
//
// The coordinator's OutputKey is "coordinator:task_board". Its text output is one of:
//   - A JSON array of tasks → dispatch should process pending tasks
//   - Anything else → treated as the final synthesis, triggers escalation
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

			// Parse the coordinator's output as a task board (tolerant of
			// markdown fences / object-wrapped boards).
			tasks, perr := parseTaskBoard(boardRaw)
			isTaskBoard := perr == nil && len(tasks) > 0

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

			normalizedBoard, _ := json.Marshal(tasks)

			hasPending := false
			for _, t := range tasks {
				if t.Status == "pending" {
					hasPending = true
					break
				}
			}

			if !hasPending {
				// Every task is settled — finish instead of re-running the
				// coordinator (which churns to max_iterations). The output
				// agent's result already streamed to the main thread.
				logger.Info().Int("tasks", len(tasks)).Msg("all tasks settled, escalating to finish")
				summary := buildResultsSummary(string(normalizedBoard))
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "check_tasks"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText(summary, genai.RoleModel),
				}
				evt.Actions.StateDelta = map[string]any{
					KeySynthesis: summary,
					KeyIteration: iteration,
					KeyTaskBoard: string(normalizedBoard),
				}
				evt.Actions.Escalate = true
				yield(evt, nil)
				return
			}

			if maxIterations > 0 && iteration >= maxIterations {
				logger.Warn().Int("iteration", iteration).Msg("max iterations reached, forcing completion")
				summary := buildResultsSummary(string(normalizedBoard))
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "check_tasks"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText(summary, genai.RoleModel),
				}
				evt.Actions.StateDelta = map[string]any{
					KeySynthesis: summary,
					KeyIteration: iteration,
					KeyTaskBoard: string(normalizedBoard),
				}
				evt.Actions.Escalate = true
				yield(evt, nil)
				return
			}

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
