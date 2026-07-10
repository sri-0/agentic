package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"agentic/internal/roster"

	"github.com/google/uuid"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// TaskArgs is the input schema for the task tool: dynamic dispatch over the
// typed subagent roster.
type TaskArgs struct {
	SubagentType string `json:"subagent_type" desc:"The subagent type to run. Must be one of the available subagent types listed in this tool's description."`
	Description  string `json:"description" desc:"A short 3-7 word label for the subtask (used as the UI card title)."`
	Prompt       string `json:"prompt" desc:"The full, self-contained instruction for the subagent. It does not see the parent conversation."`
}

// TaskResult is returned to the coordinator. The coordinator's prompt contract
// wraps Result in <task_result> when reading it.
type TaskResult struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"` // completed | failed
	Result    string `json:"result"`
}

// TaskDeps are the dependencies the task tool closes over. BuildChild constructs
// a leaf ADK agent from a roster Definition (injected from bootstrap to avoid an
// import cycle with the agent builders).
type TaskDeps struct {
	Registry       *roster.Registry
	AppName        string
	SessionService session.Service
	BuildChild     func(def *roster.Definition) (adkagent.Agent, error)
	Allowed        []string // subagent types this coordinator may dispatch (empty = all dispatchable)
}

// NewTaskTool builds the task dispatch tool. Each call resolves subagent_type
// against the registry, mints a child session (parentID = the caller's session),
// runs the child via its own Runner against the shared session service, and
// returns the child's final text. This is the opencode-style child-session
// dispatch: dynamic selection over a typed, curated roster.
func NewTaskTool(d TaskDeps) (tool.Tool, error) {
	desc := "Dispatch a subtask to a specialist subagent and get its result back. " +
		"Choose the subagent_type that best matches the work. Subagents run in isolation " +
		"(they do not see this conversation), so give a complete, self-contained prompt.\n\n" +
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

		childID := tc.SessionID() + "/" + args.SubagentType + "-" + uuid.New().String()[:8]
		content := genai.NewContentFromText(args.Prompt, genai.RoleUser)

		var finalText strings.Builder
		for ev, runErr := range r.Run(context.Background(), tc.UserID(), childID, content, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeNone}) {
			if runErr != nil {
				return TaskResult{SessionID: childID, Status: "failed", Result: "subagent error: " + runErr.Error()}, nil
			}
			if ev == nil || ev.Partial || ev.Content == nil {
				continue
			}
			for _, p := range ev.Content.Parts {
				if p.Text != "" {
					finalText.Reset()
					finalText.WriteString(p.Text)
				}
			}
		}

		return TaskResult{SessionID: childID, Status: "completed", Result: finalText.String()}, nil
	})
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
