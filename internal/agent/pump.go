package agent

import (
	"context"
	"encoding/json"

	"agentic/internal/eventlog"
	"agentic/internal/stream"
)

// PumpMode selects the closure policy for PumpEventLog.
type PumpMode int

const (
	// PumpRunAttach is used by the chat POST: the reader attaches from the run's
	// StartSeq-1, so every event it sees belongs to THIS run. It closes the HTTP
	// stream at the FIRST terminal run-status. This is what makes multi-turn
	// work: turn 2 attaches after turn 1's events and only sees turn 2.
	PumpRunAttach PumpMode = iota
	// PumpSessionFollow is used by GET /v1/sessions/{id}/stream: it replays from
	// the requested after, then stays live emitting SSE until the CLIENT
	// disconnects (ctx). It emits the AI-SDK finish framing on each terminal but
	// does NOT close the HTTP response — a follower may watch multiple runs/turns.
	PumpSessionFollow
)

// PumpEventLog drains a SeqEvent channel (from EventLog.Read) and replays each
// AgentEvent through a real stream.Encoder, reproducing the live wire output
// byte-for-byte. It is the read side of the event-sourced stream: live clients
// and reconnecting clients (via ?after=<seq>) both go through here, so the
// rendered stream is identical whether fresh or resumed.
//
// In PumpRunAttach mode it returns at the first terminal run-status (or ctx
// done). In PumpSessionFollow mode it emits finish framing on each terminal but
// keeps streaming until ctx is done.
func PumpEventLog(ctx context.Context, ch <-chan eventlog.SeqEvent, enc stream.Encoder, mode PumpMode) {
	enc.RunStarted()
	var lastUsage stream.Usage
	for {
		select {
		case se, ok := <-ch:
			if !ok {
				return
			}
			ev := se.Event
			switch ev.Type {
			case eventlog.EvHeartbeat:
				// keep-alive only
			case eventlog.EvTextDelta:
				enc.Text(ev.Text)
			case eventlog.EvReasoningDelta:
				enc.Reasoning(ev.Text)
			case eventlog.EvToolCall:
				if ev.IsOutput {
					enc.ToolCall(int64(ev.Step), toolID(ev), toolName(ev), toolArgs(ev))
				} else {
					enc.AgentToolCall(ev.Author, ev.Step, toolName(ev), toolID(ev), toolArgs(ev))
				}
			case eventlog.EvToolResult:
				if ev.IsOutput {
					enc.ToolResult(toolID(ev), toolName(ev), toolResult(ev))
				} else {
					enc.AgentToolResult(ev.Author, ev.Step, toolName(ev), toolID(ev), toResultString(toolResult(ev)))
				}
			case eventlog.EvAgentStep:
				if ev.Kind == eventlog.KindStarted {
					enc.AgentStart(ev.Author, ev.Step)
				} else {
					enc.AgentDone(ev.Author, ev.Step, ev.Duration)
				}
			case eventlog.EvAgentDelta:
				switch ev.Kind {
				case eventlog.KindReasoning:
					enc.AgentReasoning(ev.Author, ev.Step, ev.Text)
				case eventlog.KindTextDone:
					enc.AgentTextDone(ev.Author, ev.Step)
				default:
					enc.AgentText(ev.Author, ev.Step, ev.Text)
				}
			case eventlog.EvProgress:
				if ev.Author == "" {
					enc.Progress(ev.Phase, ev.Message)
				} else {
					enc.AgentProgress(ev.Phase, ev.Message, ev.Author, ev.Step)
				}
			case eventlog.EvArtifact:
				enc.Artifact(ev.Artifact)
			case eventlog.EvTaskList:
				enc.TaskList(tasksToStream(ev.Tasks))
			case eventlog.EvUsage:
				lastUsage = usageFromPayload(ev.Usage)
				enc.Usage(lastUsage, bucketsFromPayload(ev.Usage))
			case eventlog.EvMetadata:
				enc.Metadata(ev.Model, ev.Author, ev.Duration)
			case eventlog.EvQuestion:
				enc.ToolInterrupt(interruptFromPayload(ev.Question))
				if mode == PumpRunAttach {
					return // pause framing; run-attach stream closes
				}
			case eventlog.EvHITLResolved:
				// M3: re-surface the originating tool call so a fresh reader sees
				// the tool_call before its result on the resumed run — mirrors the
				// sync resume path (stream.go) which emitted a real ToolCall here.
				enc.ToolCall(int64(ev.Step), toolID(ev), toolName(ev), hitlResolvedArgs(ev))
			case eventlog.EvRunStatus:
				if ev.IsTerminal() {
					// A failed run must render as an error, not a normal "finish"
					// (which the UI shows as a Completed assistant message).
					if ev.Status == eventlog.StatusError {
						enc.RunFailed(ev.Err)
					} else {
						enc.RunFinished(lastUsage)
					}
					if mode == PumpRunAttach {
						return
					}
					// session-follow: emit finish framing but keep the HTTP
					// response open for subsequent runs/turns; reset usage so the
					// next run's finish reports its own totals.
					lastUsage = stream.Usage{}
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func toolID(ev eventlog.AgentEvent) string {
	if ev.Tool != nil {
		return ev.Tool.ID
	}
	return ""
}
func toolName(ev eventlog.AgentEvent) string {
	if ev.Tool != nil {
		return ev.Tool.Name
	}
	return ""
}
func toolArgs(ev eventlog.AgentEvent) string {
	if ev.Tool != nil {
		if s, ok := ev.Tool.Args.(string); ok {
			return s
		}
	}
	return ""
}

// hitlResolvedArgs renders the re-surfaced tool call's args. Unlike normal
// tool-call events (which store args as a pre-marshalled JSON string), the
// EvHITLResolved event carries the original tool args as a structured map
// (pending.Details), so it is marshalled here to match the sync resume path.
func hitlResolvedArgs(ev eventlog.AgentEvent) string {
	if ev.Tool == nil || ev.Tool.Args == nil {
		return ""
	}
	if s, ok := ev.Tool.Args.(string); ok {
		return s
	}
	b, err := json.Marshal(ev.Tool.Args)
	if err != nil {
		return ""
	}
	return string(b)
}
func toolResult(ev eventlog.AgentEvent) any {
	if ev.Tool != nil {
		return ev.Tool.Result
	}
	return nil
}
func toResultString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func tasksToStream(items []eventlog.TaskItem) []stream.Task {
	out := make([]stream.Task, len(items))
	for i, t := range items {
		out[i] = stream.Task{ID: t.ID, Title: t.Title, Status: t.Status, Priority: t.Priority, Agent: t.Agent}
	}
	return out
}

func usageFromPayload(p *eventlog.UsagePayload) stream.Usage {
	if p == nil {
		return stream.Usage{}
	}
	return stream.Usage{Prompt: p.PromptTokens, Completion: p.CompletionTokens, Total: p.TotalTokens, ContextWindow: p.ContextWindow}
}

func bucketsFromPayload(p *eventlog.UsagePayload) []stream.Bucket {
	if p == nil {
		return nil
	}
	out := make([]stream.Bucket, len(p.Breakdown))
	for i, b := range p.Breakdown {
		out[i] = stream.Bucket{Label: b.Label, Tokens: b.Tokens}
	}
	return out
}

func interruptFromPayload(q *eventlog.QuestionPayload) stream.Interrupt {
	if q == nil {
		return stream.Interrupt{}
	}
	i := stream.Interrupt{ToolCallID: q.ToolCallID, ToolName: q.ToolName, Prompt: q.Prompt}
	if q.Details != nil {
		i.Details = q.Details["details"]
		if tid, ok := q.Details["thread_id"].(string); ok {
			i.ThreadID = tid
		}
	}
	return i
}
