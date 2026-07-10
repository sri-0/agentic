package coordinator

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

// boardEvent builds a content-less event carrying the current task board as a
// state delta, so the stream loop can surface live pending→running→done
// transitions in the UI task bar. Must be yielded from the dispatch iterator
// goroutine only (never from a worker goroutine).
func boardEvent(ctx agent.InvocationContext, tasks []Task) *session.Event {
	b, _ := json.Marshal(tasks)
	evt := session.NewEvent(ctx.InvocationID())
	evt.Author = "dispatch"
	evt.Actions.StateDelta = map[string]any{KeyTaskBoard: string(b)}
	return evt
}

// dispatchRun is a code agent that reads pending tasks from the task board,
// runs the assigned worker agents in parallel, and writes results back.
func dispatchRun(
	workers map[string]agent.Agent,
	maxParallel int,
	outputAgent string,
	logger zerolog.Logger,
) func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
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

			tasks, err := parseTaskBoard(raw)
			if err != nil {
				yield(nil, fmt.Errorf("parse task board: %w", err))
				return
			}

			// Output agent (writer) runs in a LAST phase so the final answer
			// streams after every other worker finishes.
			var normal, last []int
			for i, t := range tasks {
				if t.Status != "pending" {
					continue
				}
				if outputAgent != "" && t.Worker == outputAgent {
					last = append(last, i)
				} else {
					normal = append(normal, i)
				}
			}

			if len(normal)+len(last) == 0 {
				evt := session.NewEvent(ctx.InvocationID())
				evt.Author = "dispatch"
				evt.LLMResponse = model.LLMResponse{
					Content: genai.NewContentFromText("All tasks already completed.", genai.RoleModel),
				}
				yield(evt, nil)
				return
			}

			logger.Info().Int("normal", len(normal)).Int("final", len(last)).Int("max_parallel", maxParallel).Msg("dispatching workers")

			runBatch := func(batch []int) bool {
				for _, idx := range batch {
					tasks[idx].Status = "running"
				}
				if !yield(boardEvent(ctx, tasks), nil) {
					return true
				}

				type workerResult struct {
					taskIdx int
					result  string
					err     error
				}
				results := make([]workerResult, len(batch))
				events := make(chan *session.Event, 64)
				var wg sync.WaitGroup

				for bi, taskIdx := range batch {
					wg.Add(1)
					go func(bi, taskIdx int) {
						defer wg.Done()
						task := tasks[taskIdx]
						instanceID := task.Worker + "#" + task.ID
						w, ok := workers[task.Worker]
						if !ok {
							results[bi] = workerResult{taskIdx: taskIdx, err: fmt.Errorf("unknown worker: %s", task.Worker)}
							return
						}
						logger.Info().Str("worker", instanceID).Str("task", task.ID).Msg("worker started")
						var lastText string
						for event, err := range w.Run(ctx) {
							if err != nil {
								results[bi] = workerResult{taskIdx: taskIdx, err: err}
								return
							}
							event.Author = instanceID
							events <- event
							if event.Content != nil {
								for _, p := range event.Content.Parts {
									if p.Text != "" {
										lastText = p.Text
									}
								}
							}
						}
						logger.Info().Str("worker", instanceID).Int("result_len", len(lastText)).Msg("worker done")
						results[bi] = workerResult{taskIdx: taskIdx, result: lastText}
					}(bi, taskIdx)
				}

				go func() { wg.Wait(); close(events) }()

				stopped := false
				for event := range events {
					if stopped {
						continue
					}
					if !yield(event, nil) {
						stopped = true
					}
				}
				if stopped {
					return true
				}

				for _, wr := range results {
					if wr.err != nil {
						tasks[wr.taskIdx].Status = "failed"
						tasks[wr.taskIdx].Result = wr.err.Error()
						logger.Error().Err(wr.err).Str("task", tasks[wr.taskIdx].ID).Msg("worker failed")
					} else {
						tasks[wr.taskIdx].Status = "done"
						tasks[wr.taskIdx].Result = wr.result
					}
				}
				return !yield(boardEvent(ctx, tasks), nil)
			}

			runPhase := func(indices []int) bool {
				for start := 0; start < len(indices); start += maxParallel {
					end := start + maxParallel
					if end > len(indices) {
						end = len(indices)
					}
					if runBatch(indices[start:end]) {
						return true
					}
				}
				return false
			}

			if runPhase(normal) {
				return
			}
			if runPhase(last) {
				return
			}

			updatedBoard, _ := json.Marshal(tasks)

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
