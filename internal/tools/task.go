package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"agentic/internal/eventlog"
	"agentic/internal/roster"

	"github.com/google/uuid"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// defaultMaxParallelWorkers bounds concurrent background children per coordinator
// (a semaphore shared across a task-tool instance). Sensible default; configurable
// later via TaskDeps.MaxParallel.
const defaultMaxParallelWorkers = 4

// TaskArgs is the input schema for the task tool: dynamic dispatch over the
// typed subagent roster.
type TaskArgs struct {
	SubagentType string `json:"subagent_type" desc:"The subagent type to run. Must be one of the available subagent types listed in this tool's description."`
	Description  string `json:"description" desc:"A short 3-7 word label for the subtask (used as the UI card title)."`
	Prompt       string `json:"prompt" desc:"The full, self-contained instruction for the subagent. It does not see the parent conversation."`
	Background   bool   `json:"background,omitempty" desc:"If true, dispatch the subagent asynchronously and return immediately with status \"running\"; join later with task_join. Prefer this to run several subagents in parallel."`
}

// TaskResult is returned to the coordinator. The coordinator's prompt contract
// wraps Result in <task_result> when reading it.
type TaskResult struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"` // completed | running | failed
	Result    string `json:"result"`
}

// TaskDeps are the dependencies the task tool closes over. BuildChild constructs
// a leaf ADK agent from a roster Definition (injected from bootstrap to avoid an
// import cycle with the agent builders).
type TaskDeps struct {
	Registry       *roster.Registry
	AppName        string
	SessionService session.Service
	// EventLog is the parent-session durable log; child events are streamed into
	// it (attributed to the sub-agent) so the parent UI renders live agent cards.
	EventLog eventlog.EventLog
	// BuildChild constructs a child agent from a definition. denyDispatch=true
	// means the child must NOT receive question/task tools (default deny for
	// spawned leaves; a child whose def opts into sub-dispatch may be built with
	// denyDispatch=false by the caller).
	BuildChild  func(def *roster.Definition) (adkagent.Agent, error)
	Allowed     []string   // subagent types this coordinator may dispatch (empty = all dispatchable)
	MaxParallel int        // 0 → defaultMaxParallelWorkers
	Hub         *TaskHub   // shared background-handle registry (task + task_join)
}

// TaskHub tracks background children so task_join can wait on them. It is shared
// between the task and task_join tools built for one coordinator instance and is
// safe for concurrent use.
type TaskHub struct {
	mu    sync.Mutex
	jobs  map[string]*taskJob            // keyed by child session id
	tasks map[string][]eventlog.TaskItem // spawn-synthesised task list, keyed by parent session id
	sem   chan struct{}
}

type taskJob struct {
	done   chan struct{}
	result TaskResult
}

// NewTaskHub returns a hub bounding concurrency to maxParallel (<=0 → default).
func NewTaskHub(maxParallel int) *TaskHub {
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallelWorkers
	}
	return &TaskHub{
		jobs:  make(map[string]*taskJob),
		tasks: make(map[string][]eventlog.TaskItem),
		sem:   make(chan struct{}, maxParallel),
	}
}

// upsertTask appends or updates the spawn-synthesised task row for a child and
// returns the current monotonic snapshot for the parent session. A settled
// status (done/error) is never regressed back to running.
func (h *TaskHub) upsertTask(parentID string, item eventlog.TaskItem) []eventlog.TaskItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.tasks[parentID]
	found := false
	for i := range list {
		if list[i].ID == item.ID {
			if !settled(list[i].Status) {
				list[i].Status = item.Status
			}
			if item.Title != "" {
				list[i].Title = item.Title
			}
			if item.Agent != "" {
				list[i].Agent = item.Agent
			}
			found = true
			break
		}
	}
	if !found {
		list = append(list, item)
	}
	h.tasks[parentID] = list
	out := make([]eventlog.TaskItem, len(list))
	copy(out, list)
	return out
}

func settled(status string) bool {
	return status == "done" || status == "error" || status == "completed" || status == "failed"
}

func (h *TaskHub) register(id string) *taskJob {
	j := &taskJob{done: make(chan struct{})}
	h.mu.Lock()
	h.jobs[id] = j
	h.mu.Unlock()
	return j
}

func (h *TaskHub) finish(id string, res TaskResult) {
	h.mu.Lock()
	j := h.jobs[id]
	h.mu.Unlock()
	if j == nil {
		return
	}
	j.result = res
	close(j.done)
}

// wait blocks until the job for id completes (or ctx is cancelled) and returns
// its result. ok=false if id is unknown.
func (h *TaskHub) wait(ctx context.Context, id string) (TaskResult, bool) {
	h.mu.Lock()
	j := h.jobs[id]
	h.mu.Unlock()
	if j == nil {
		return TaskResult{}, false
	}
	select {
	case <-j.done:
		return j.result, true
	case <-ctx.Done():
		return TaskResult{SessionID: id, Status: "running", Result: "join cancelled before completion"}, true
	}
}

// NewTaskTool builds the task dispatch tool. Each call resolves subagent_type
// against the registry, mints a child session (parentID = the caller's session),
// runs the child via its own Runner against the shared session service with
// StreamingModeSSE, and streams the child's events into the PARENT session's log
// (attributed to the sub-agent) so the UI renders live agent cards. Returns the
// child's final text plus the child session id.
func NewTaskTool(d TaskDeps) (tool.Tool, error) {
	if d.Hub == nil {
		d.Hub = NewTaskHub(d.MaxParallel)
	}
	desc := "Dispatch a subtask to a specialist subagent and get its result back. " +
		"Choose the subagent_type that best matches the work. Subagents run in isolation " +
		"(they do not see this conversation), so give a complete, self-contained prompt. " +
		"Set background:true to run several subagents in parallel, then call task_join with " +
		"the returned session_ids to collect their results.\n\n" +
		d.Registry.Manifest(d.Allowed)

	return functiontool.New(functiontool.Config{
		Name:        "task",
		Description: desc,
	}, func(tc tool.Context, args TaskArgs) (TaskResult, error) {
		def, ok := d.Registry.Get(args.SubagentType)
		if !ok || !d.isAllowed(args.SubagentType) || def.Config() == nil {
			return TaskResult{Status: "failed", Result: fmt.Sprintf(
				"unknown subagent_type %q. Valid types: %s",
				args.SubagentType, strings.Join(d.dispatchableNames(), ", "))}, nil
		}

		child, err := d.BuildChild(def)
		if err != nil {
			return TaskResult{Status: "failed", Result: "failed to build subagent: " + err.Error()}, nil
		}

		r, err := runner.New(runner.Config{AppName: d.AppName, Agent: child, SessionService: d.SessionService})
		if err != nil {
			return TaskResult{Status: "failed", Result: "failed to start subagent runner: " + err.Error()}, nil
		}

		// M5: use ':' (mux-safe) not '/', so the child is reachable via
		// /v1/sessions/{id}. Format: parent:type-shortid.
		parentID := tc.SessionID()
		short := uuid.New().String()[:8]
		childID := parentID + ":" + args.SubagentType + "-" + short
		label := args.SubagentType + "#" + short // multi-instance disambiguation

		run := func(ctx context.Context) TaskResult {
			return d.runChild(ctx, r, tc.UserID(), parentID, childID, label, args)
		}

		if args.Background {
			d.Hub.register(childID)
			// M2: derive from a background context so the child outlives the tool
			// call but is still bounded by the semaphore.
			go func() {
				d.Hub.sem <- struct{}{}
				defer func() { <-d.Hub.sem }()
				res := run(context.WithoutCancel(tc))
				d.Hub.finish(childID, res)
			}()
			return TaskResult{SessionID: childID, Status: "running"}, nil
		}

		// Foreground: bound by the semaphore too, and derive the run context from
		// the tool context so cancelling the parent cancels the child (M2 fix).
		d.Hub.sem <- struct{}{}
		defer func() { <-d.Hub.sem }()
		return run(tc), nil
	})
}

// runChild executes one child run to completion, streaming its events into the
// parent log, and returns the collected final text.
func (d TaskDeps) runChild(ctx context.Context, r *runner.Runner, userID, parentID, childID, label string, args TaskArgs) TaskResult {
	sink := &childLogSink{ctx: context.WithoutCancel(ctx), log: d.EventLog, parentID: parentID}

	// The runner requires the session to exist. Mint the child session on the
	// shared persisted service, seeded with parent/subagent metadata so it is a
	// first-class, resumable session reachable via /v1/sessions/{childID}.
	if d.SessionService != nil {
		if _, err := d.SessionService.Create(ctx, &session.CreateRequest{
			AppName:   d.AppName,
			UserID:    userID,
			SessionID: childID,
			State: map[string]any{
				"_meta:parentID":     parentID,
				"_meta:subagentType": args.SubagentType,
				"_meta:description":  args.Description,
			},
		}); err != nil && !strings.Contains(err.Error(), "already exists") {
			return TaskResult{SessionID: childID, Status: "failed", Result: "failed to create subagent session: " + err.Error()}
		}
	}
	// Parent-log step index for this child instance. A monotonic-ish value based
	// on the current head keeps sub-agent cards ordered without a global counter.
	step := d.nextStep(parentID)

	start := time.Now()
	sink.append(eventlog.AgentEvent{Type: eventlog.EvAgentStep, Kind: eventlog.KindStarted,
		Author: label, SubagentType: args.SubagentType, SessionID: childID, Step: step})

	// Spawn-synthesised task list: append a running row for this child and emit
	// the full parent snapshot so the UI task bar reflects the swarm.
	if d.Hub != nil {
		snap := d.Hub.upsertTask(parentID, eventlog.TaskItem{
			ID: childID, Title: args.Description, Status: "running", Agent: args.SubagentType})
		sink.append(eventlog.AgentEvent{Type: eventlog.EvTaskList, Tasks: snap})
	}

	content := genai.NewContentFromText(args.Prompt, genai.RoleUser)
	st := &childStreamState{}
	var runErr error
	for ev, err := range r.Run(ctx, userID, childID, content, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}) {
		if err != nil {
			runErr = err
			break
		}
		for _, out := range translateChildEvent(ev, label, args.SubagentType, childID, step, st) {
			sink.append(out)
		}
	}

	sink.append(eventlog.AgentEvent{Type: eventlog.EvAgentStep, Kind: eventlog.KindDone,
		Author: label, SubagentType: args.SubagentType, SessionID: childID, Step: step,
		Duration: time.Since(start).Milliseconds()})

	// "completed" (not "done") so the UI TaskBar, which counts status === "completed"
	// as finished, shows a settled child as done (e.g. 1/1, not 0/1).
	finalStatus := "completed"
	if runErr != nil || st.blocked {
		finalStatus = "error"
	}
	if d.Hub != nil {
		snap := d.Hub.upsertTask(parentID, eventlog.TaskItem{ID: childID, Status: finalStatus})
		sink.append(eventlog.AgentEvent{Type: eventlog.EvTaskList, Tasks: snap})
	}

	if runErr != nil {
		return TaskResult{SessionID: childID, Status: "failed", Result: "subagent error: " + runErr.Error()}
	}
	if st.blocked {
		// The child tried to ask a question but has no resume path here.
		return TaskResult{SessionID: childID, Status: "failed",
			Result: "subagent attempted to ask a question, which is not supported for dispatched subagents. " +
				"Provide a complete, self-contained prompt instead. Partial output: " + st.finalText}
	}
	return TaskResult{SessionID: childID, Status: "completed", Result: st.finalText}
}

// nextStep returns a step index for a child card, derived from the parent log
// head so successive children order after prior events. Falls back to a fixed
// value if the log is unavailable.
func (d TaskDeps) nextStep(parentID string) int {
	if d.EventLog == nil {
		return 1
	}
	head, err := d.EventLog.Head(context.Background(), parentID)
	if err != nil {
		return 1
	}
	return int(head) + 1
}

// NewTaskJoinTool builds the task_join tool, which blocks until the given
// background children finish and returns their <task_result>s. It shares the
// TaskHub with the task tool.
func NewTaskJoinTool(hub *TaskHub) (tool.Tool, error) {
	if hub == nil {
		hub = NewTaskHub(0)
	}
	return functiontool.New(functiontool.Config{
		Name: "task_join",
		Description: "Wait for one or more background subagents (started via task with background:true) " +
			"to finish, and return their results. Pass the session_ids returned by those task calls.",
	}, func(tc tool.Context, args TaskJoinArgs) (TaskJoinResult, error) {
		out := TaskJoinResult{Results: make([]TaskResult, 0, len(args.SessionIDs))}
		for _, id := range args.SessionIDs {
			res, ok := hub.wait(tc, id)
			if !ok {
				res = TaskResult{SessionID: id, Status: "failed", Result: "unknown session_id (not a background task)"}
			}
			out.Results = append(out.Results, res)
		}
		return out, nil
	})
}

// TaskJoinArgs is the input schema for task_join.
type TaskJoinArgs struct {
	SessionIDs []string `json:"session_ids" desc:"The session_ids returned by prior background task calls to wait for."`
}

// TaskJoinResult carries the joined children's results.
type TaskJoinResult struct {
	Results []TaskResult `json:"results"`
}

func (d TaskDeps) isAllowed(name string) bool {
	if len(d.Allowed) == 0 {
		if def, ok := d.Registry.Get(name); ok {
			return def.Mode == roster.ModeSubagent || def.Mode == roster.ModeAll
		}
		return false
	}
	return slices.Contains(d.Allowed, name)
}

func (d TaskDeps) dispatchableNames() []string {
	if len(d.Allowed) > 0 {
		return d.Allowed
	}
	var names []string
	for _, def := range d.Registry.Dispatchable() {
		names = append(names, def.Name)
	}
	return names
}
