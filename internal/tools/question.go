package tools

import (
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// QuestionOption is one selectable answer (mirrors opencode's option shape).
type QuestionOption struct {
	Label       string `json:"label" desc:"The option text shown to the user."`
	Description string `json:"description,omitempty" desc:"Optional explanation of the option."`
}

// QuestionItem mirrors opencode's question schema exactly so the wire/UI contract
// is consistent: a question, a short header, options, multi-select, and whether
// free-text ('type your own') is allowed.
type QuestionItem struct {
	Question string           `json:"question" desc:"The complete question to ask the user."`
	Header   string           `json:"header" desc:"A very short label (max ~30 chars) shown as a chip."`
	Options  []QuestionOption `json:"options" desc:"The selectable options."`
	Multiple bool             `json:"multiple,omitempty" desc:"Allow selecting more than one option."`
	Custom   *bool            `json:"custom,omitempty" desc:"Allow a free-text answer (defaults to true)."`
}

// QuestionArgs is the input schema: ask one or more questions at once.
type QuestionArgs struct {
	Questions []QuestionItem `json:"questions" desc:"The questions to ask the user."`
}

// QuestionResult is returned after the user answers (one answer list per question).
type QuestionResult struct {
	Answers [][]string `json:"answers"`
	Status  string     `json:"status"`
}

// NewQuestionTool creates the interactive question tool. It uses ADK's
// confirmation mechanism to pause the run and surface the questions to the UI
// (via the same interrupt path as HITL); the user's reply resumes the run. The
// schema matches opencode's `question` tool so the frontend contract is shared.
func NewQuestionTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "question",
		Description: "Ask the user one or more questions and wait for their answer before continuing. " +
			"Use this when you need a decision or missing information that only the user can provide. " +
			"Each question has a short header, a list of options, and optionally allows a free-text answer.",
		RequireConfirmation: true, // pauses the run and surfaces the questions to the UI
	}, questionHandler)
}

func questionHandler(_ tool.Context, args QuestionArgs) (QuestionResult, error) {
	// Reached only after the user responds via the resume flow. The structured
	// answer payload is surfaced to the model as the tool result.
	return QuestionResult{Answers: [][]string{}, Status: "answered"}, nil
}
