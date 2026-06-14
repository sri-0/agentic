package tools

import (
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// TodoItem is a single structured todo. Status is one of:
// pending | in_progress | completed | cancelled. Priority (optional) is one of:
// high | medium | low.
type TodoItem struct {
	Content  string `json:"content" desc:"The todo text."`
	Status   string `json:"status" desc:"One of: pending, in_progress, completed, cancelled."`
	Priority string `json:"priority,omitempty" desc:"Optional priority: high, medium, or low."`
}

// TodoWriteArgs is the input schema for the todowrite tool. Each call replaces
// the entire todo list (snapshot semantics).
type TodoWriteArgs struct {
	Todos []TodoItem `json:"todos" desc:"The full todo list. This replaces any previous list."`
}

// TodoWriteResult is returned to the agent and is also the payload the streaming
// layer reads to emit the task_list CUSTOM event to the UI.
type TodoWriteResult struct {
	Todos  []TodoItem `json:"todos"`
	Status string     `json:"status"`
}

// NewTodoWriteTool creates the built-in todowrite tool. Agents call it to
// surface a structured todo list to the UI. The tool has no side effects beyond
// returning the todos snapshot; the agent streaming loop detects this tool's
// response and emits a task_list CUSTOM event.
func NewTodoWriteTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "todowrite",
		Description: "Write a structured todo list to the UI. Each call replaces the entire list (snapshot). Use it to plan and track multi-step work.",
	}, todoWriteHandler)
}

func todoWriteHandler(_ tool.Context, args TodoWriteArgs) (TodoWriteResult, error) {
	todos := args.Todos
	if todos == nil {
		todos = []TodoItem{}
	}
	return TodoWriteResult{
		Todos:  todos,
		Status: "written",
	}, nil
}
