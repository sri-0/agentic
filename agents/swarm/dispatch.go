package swarm

import (
	"encoding/json"
	"fmt"
	"iter"
	"sync"

	"github.com/rs/zerolog"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// dispatchRun is a code agent that reads pending tasks from the task board,
// runs the assigned worker agents in parallel, and writes results back.
func dispatchRun(
	workers map[string]agent.Agent,
	maxParallel int,
	logger zerolog.Logger,
) func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			// Load task board
			raw := stateString(ctx, KeyTaskBoard)
			if raw == "" {
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "dispatch"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText("No tasks on the board.", genai.RoleModel),
				}
				yield(evt, nil)
				return
			}

			var tasks []Task
			if err := json.Unmarshal([]byte(raw), &tasks); err != nil {
				yield(nil, fmt.Errorf("parse task board: %w", err))
				return
			}

			// Find pending tasks
			var pending []int
			for i, t := range tasks {
				if t.Status == "pending" {
					pending = append(pending, i)
				}
			}

			if len(pending) == 0 {
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "dispatch"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText("All tasks already completed.", genai.RoleModel),
				}
				yield(evt, nil)
				return
			}

			logger.Info().Int("pending", len(pending)).Int("max_parallel", maxParallel).Msg("dispatching workers")

			// Process in batches of maxParallel
			for batchStart := 0; batchStart < len(pending); batchStart += maxParallel {
				batchEnd := batchStart + maxParallel
				if batchEnd > len(pending) {
					batchEnd = len(pending)
				}
				batch := pending[batchStart:batchEnd]

				// Mark batch tasks as running
				for _, idx := range batch {
					tasks[idx].Status = "running"
				}

				type workerResult struct {
					taskIdx int
					result  string
					err     error
					events  []*session.Event
				}

				results := make([]workerResult, len(batch))
				var wg sync.WaitGroup

				for bi, taskIdx := range batch {
					wg.Add(1)
					go func(bi, taskIdx int) {
						defer wg.Done()

						task := tasks[taskIdx]
						w, ok := workers[task.Worker]
						if !ok {
							results[bi] = workerResult{
								taskIdx: taskIdx,
								err:     fmt.Errorf("unknown worker: %s", task.Worker),
							}
							return
						}

						logger.Info().Str("worker", task.Worker).Str("task", task.ID).Msg("worker started")

						// Set the task input in state so the worker can read it
						inputEvt := session.NewEvent(ctx.InvocationID())
						inputEvt.Author = "dispatch"
						inputEvt.Actions.StateDelta = map[string]any{
							"swarm:current_task": task.Input,
						}

						var events []*session.Event
						var lastText string

						// Run worker — collect events
						for event, err := range w.Run(ctx) {
							if err != nil {
								results[bi] = workerResult{taskIdx: taskIdx, err: err}
								return
							}
							events = append(events, event)
							// Extract text output
							if event.Content != nil {
								for _, p := range event.Content.Parts {
									if p.Text != "" {
										lastText = p.Text
									}
								}
							}
						}

						logger.Info().Str("worker", task.Worker).Str("task", task.ID).Int("result_len", len(lastText)).Msg("worker done")

						results[bi] = workerResult{
							taskIdx: taskIdx,
							result:  lastText,
							events:  events,
						}
					}(bi, taskIdx)
				}

				wg.Wait()

				// Forward worker events and update task board
				for _, wr := range results {
					// Forward worker events to the yield
					for _, evt := range wr.events {
						if !yield(evt, nil) {
							return
						}
					}

					if wr.err != nil {
						tasks[wr.taskIdx].Status = "failed"
						tasks[wr.taskIdx].Result = wr.err.Error()
						logger.Error().Err(wr.err).Str("task", tasks[wr.taskIdx].ID).Msg("worker failed")
					} else {
						tasks[wr.taskIdx].Status = "done"
						tasks[wr.taskIdx].Result = wr.result
					}
				}
			}

			// Write updated task board back to state
			updatedBoard, _ := json.Marshal(tasks)

			// Build summary
			var doneCount, failedCount int
			for _, t := range tasks {
				switch t.Status {
				case "done":
					doneCount++
				case "failed":
					failedCount++
				}
			}

			evt := session.NewEvent(ctx.InvocationID())
			evt.Author = "dispatch"
			evt.LLMResponse = model.LLMResponse{
				Content: genai.NewContentFromText(
					fmt.Sprintf("Dispatch complete: %d done, %d failed out of %d total tasks.",
						doneCount, failedCount, len(tasks)),
					genai.RoleModel,
				),
			}
			evt.Actions.StateDelta = map[string]any{
				KeyTaskBoard: string(updatedBoard),
			}
			yield(evt, nil)
		}
	}
}
