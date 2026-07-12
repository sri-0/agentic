package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/adk/tool/toolconfirmation"
)

// adkRoundTrip reproduces exactly what ADK's request_confirmation_processor does
// with a FunctionResponse.Response map: json.Marshal(Response) then
// json.Unmarshal into a toolconfirmation.ToolConfirmation. This is the seam that
// carries the user's answers from the resume handler to the question tool, so we
// verify the whole encode→decode chain end to end (minus the live model call).
func adkRoundTrip(t *testing.T, response map[string]any) *toolconfirmation.ToolConfirmation {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var tc toolconfirmation.ToolConfirmation
	if err := json.Unmarshal(raw, &tc); err != nil {
		t.Fatalf("unmarshal into ToolConfirmation: %v", err)
	}
	return &tc
}

// buildConfirmationResponse mirrors the shape the coordinator's Resume builds
// (internal/agent/coordinator.go buildConfirmationResponse). Kept local to avoid
// an import cycle; the keys must match the tool's decode constants.
func fakeConfirmationResponse(approved bool, answers [][]string, text string) map[string]any {
	resp := map[string]any{"confirmed": approved}
	if len(answers) > 0 || text != "" {
		payload := map[string]any{}
		if len(answers) > 0 {
			payload[confirmKeyAnswers] = answers
		}
		if text != "" {
			payload[confirmKeyText] = text
		}
		resp[confirmKeyPayload] = payload
	}
	return resp
}

func TestQuestionResult_AnswersRoundTripThroughADKPayload(t *testing.T) {
	questions := []QuestionItem{
		{Question: "Which environment?", Header: "env"},
		{Question: "Confirm rollout?", Header: "rollout"},
	}
	want := [][]string{{"staging"}, {"yes", "notify me"}}

	// Encode as the resume path does, round-trip through ADK's JSON decode.
	tc := adkRoundTrip(t, fakeConfirmationResponse(true, want, "ship it Friday"))

	if !tc.Confirmed {
		t.Fatal("expected Confirmed=true after round-trip")
	}

	res := buildQuestionResult(tc, questions)
	if res.Status != "answered" {
		t.Fatalf("status = %q, want answered", res.Status)
	}
	if !reflect.DeepEqual(res.Answers, want) {
		t.Fatalf("answers = %#v, want %#v", res.Answers, want)
	}
	// Model-facing summary must actually contain the chosen labels (opencode style).
	for _, label := range []string{"staging", "yes", "notify me", "ship it Friday"} {
		if !strings.Contains(res.Summary, label) {
			t.Errorf("summary %q missing label %q", res.Summary, label)
		}
	}
	if !strings.Contains(res.Summary, "Which environment?") {
		t.Errorf("summary %q missing question text", res.Summary)
	}
}

func TestQuestionResult_Dismissed(t *testing.T) {
	// No confirmation at all.
	res := buildQuestionResult(nil, nil)
	if res.Status != "dismissed" {
		t.Fatalf("nil conf: status = %q, want dismissed", res.Status)
	}

	// Explicitly not confirmed.
	tc := adkRoundTrip(t, fakeConfirmationResponse(false, nil, ""))
	res = buildQuestionResult(tc, nil)
	if res.Status != "dismissed" {
		t.Fatalf("unconfirmed: status = %q, want dismissed", res.Status)
	}
	if len(res.Answers) != 0 {
		t.Fatalf("dismissed answers = %#v, want empty", res.Answers)
	}
}

func TestQuestionResult_ConfirmedNoAnswers(t *testing.T) {
	// A plain HITL approve (write_database style) with no answers payload: the
	// tool should report answered with an empty answer set, no panic.
	tc := adkRoundTrip(t, fakeConfirmationResponse(true, nil, ""))
	res := buildQuestionResult(tc, nil)
	if res.Status != "answered" {
		t.Fatalf("status = %q, want answered", res.Status)
	}
	if len(res.Answers) != 0 {
		t.Fatalf("answers = %#v, want empty", res.Answers)
	}
}

func TestParseConfirmationPayload_ToleratesJSONShapes(t *testing.T) {
	// After a JSON round-trip, [][]string becomes []any of []any of string.
	raw := `{"answers":[["a","b"],["c"]],"text":"hi"}`
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	answers, text := parseConfirmationPayload(payload)
	want := [][]string{{"a", "b"}, {"c"}}
	if !reflect.DeepEqual(answers, want) {
		t.Fatalf("answers = %#v, want %#v", answers, want)
	}
	if text != "hi" {
		t.Fatalf("text = %q, want hi", text)
	}
}
