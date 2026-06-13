package tools

import (
	"github.com/google/uuid"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// EmitArtifactArgs is the input schema for the emit_artifact tool.
type EmitArtifactArgs struct {
	ID       string `json:"id,omitempty" desc:"Optional artifact id; reuse the same id to update an existing artifact. Auto-generated if omitted."`
	Title    string `json:"title" desc:"Human-readable artifact title."`
	Kind     string `json:"kind" desc:"One of: markdown, code, html, json."`
	Content  string `json:"content" desc:"The artifact body."`
	Language string `json:"language,omitempty" desc:"Optional source language when kind is code (e.g. python, go)."`
}

// EmitArtifactResult is returned to the agent and is also the payload the
// streaming layer reads to emit the artifact CUSTOM event to the UI.
type EmitArtifactResult struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Content  string `json:"content"`
	Language string `json:"language,omitempty"`
	Status   string `json:"status"`
}

// NewEmitArtifactTool creates the built-in emit_artifact tool. Agents call it
// to push an artifact (markdown/code/html/json) into the UI sidepanel. The tool
// has no side effects beyond returning the artifact payload; the agent
// streaming loop detects this tool's response and emits the artifact event.
func NewEmitArtifactTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "emit_artifact",
		Description: "Push an artifact (markdown, code, html, or json) to the UI sidepanel. Reuse the same id to update a previously emitted artifact.",
	}, emitArtifactHandler)
}

func emitArtifactHandler(_ tool.Context, args EmitArtifactArgs) (EmitArtifactResult, error) {
	id := args.ID
	if id == "" {
		id = uuid.New().String()
	}
	kind := args.Kind
	if kind == "" {
		kind = "markdown"
	}
	return EmitArtifactResult{
		ID:       id,
		Title:    args.Title,
		Kind:     kind,
		Content:  args.Content,
		Language: args.Language,
		Status:   "emitted",
	}, nil
}
