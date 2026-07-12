package tools

import (
	"fmt"
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/adk/tool/toolconfirmation"
)

// Confirmation payload keys. These MUST match the keys the resume path packs
// into the ADK FunctionResponse (internal/agent/coordinator.go
// buildConfirmationResponse): the run coordinator encodes answers under
// payload["answers"]; here we decode them back out of
// ctx.ToolConfirmation().Payload. Kept as untyped map access because ADK
// round-trips the Response map through JSON, so Payload arrives as
// map[string]any with []any / []string leaves.
const (
	confirmKeyPayload = "payload"
	confirmKeyAnswers = "answers"
	confirmKeyText    = "text"
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

// QuestionResult is returned after the user answers (one answer list per
// question). Summary is the model-facing string (opencode format) so the model
// actually sees the user's choices; Answers is the structured form for the UI.
type QuestionResult struct {
	Answers [][]string `json:"answers"`
	Status  string     `json:"status"`
	Summary string     `json:"summary,omitempty"`
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

// questionHandler runs only after the user replies via the resume flow. The
// user's answers ride the ADK tool-confirmation payload, so we read them from
// ctx.ToolConfirmation() and return them as the tool result. Alongside the
// structured Answers we build a model-facing summary string (opencode format)
// so the model can actually reason over the user's choices.
func questionHandler(ctx tool.Context, args QuestionArgs) (QuestionResult, error) {
	return buildQuestionResult(ctx.ToolConfirmation(), args.Questions), nil
}

// buildQuestionResult is the pure core of questionHandler, split out so it can
// be unit-tested without faking the whole tool.Context: given the ADK
// confirmation and the asked questions, produce the tool result (structured
// answers + model-facing summary).
func buildQuestionResult(conf *toolconfirmation.ToolConfirmation, questions []QuestionItem) QuestionResult {
	// Dismissed / no confirmation: surface a graceful "not answered" result
	// instead of pretending the user picked something.
	if conf == nil || !conf.Confirmed {
		return QuestionResult{
			Answers: [][]string{},
			Status:  "dismissed",
			Summary: "The user dismissed the question(s) without answering. Continue without their input or ask again.",
		}
	}

	answers, text := parseConfirmationPayload(conf.Payload)
	return QuestionResult{
		Answers: answers,
		Status:  "answered",
		Summary: formatAnswerSummary(questions, answers, text),
	}
}

// parseConfirmationPayload extracts the answers ([][]string) and optional
// free-text from the ADK confirmation payload. ADK round-trips the confirmation
// Response through JSON, so Payload is a map[string]any and the answers arrive
// as []any of []any of string (or []string when injected directly by a test).
func parseConfirmationPayload(payload any) (answers [][]string, text string) {
	m, ok := payload.(map[string]any)
	if !ok {
		return [][]string{}, ""
	}
	if t, ok := m[confirmKeyText].(string); ok {
		text = t
	}
	answers = toStringMatrix(m[confirmKeyAnswers])
	return answers, text
}

// toStringMatrix coerces the JSON-decoded answers value into [][]string,
// tolerating both the native [][]string (test-injected) and the []any/[]any
// shape that survives an ADK JSON round-trip.
func toStringMatrix(v any) [][]string {
	switch a := v.(type) {
	case [][]string:
		return a
	case []any:
		out := make([][]string, 0, len(a))
		for _, row := range a {
			out = append(out, toStringSlice(row))
		}
		return out
	case []string:
		return [][]string{a}
	default:
		return [][]string{}
	}
}

func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			} else {
				out = append(out, fmt.Sprint(item))
			}
		}
		return out
	default:
		return nil
	}
}

// formatAnswerSummary renders the opencode-style model-facing string:
//
//	User has answered your questions: "Q"="label, label", ...
//	You can now continue with the user's answers in mind.
func formatAnswerSummary(questions []QuestionItem, answers [][]string, text string) string {
	var parts []string
	for i, ans := range answers {
		q := ""
		if i < len(questions) {
			q = questions[i].Question
		}
		parts = append(parts, fmt.Sprintf("%q=%q", q, strings.Join(ans, ", ")))
	}
	summary := "User has answered your questions"
	if len(parts) > 0 {
		summary += ": " + strings.Join(parts, ", ")
	}
	if text != "" {
		summary += fmt.Sprintf(". They also added: %q", text)
	}
	summary += ". You can now continue with the user's answers in mind."
	return summary
}
