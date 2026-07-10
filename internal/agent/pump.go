package agent

import (
	"context"

	"agentic/internal/eventlog"
	"agentic/internal/stream"
)

// PumpEventLog drains a SeqEvent channel (from EventLog.Read) and replays each
// AgentEvent through a real stream.Encoder, reproducing the live wire output
// byte-for-byte. It is the read side of the event-sourced stream: live clients
// and reconnecting clients (via ?after=<seq>) both go through here, so the
// rendered stream is identical whether fresh or resumed.
//
// It returns when the channel closes (terminal status reached) or ctx is done.
func PumpEventLog(ctx context.Context, ch <-chan eventlog.SeqEvent, enc stream.Encoder) {
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
				return // pause framing; stream closes
			case eventlog.EvRunStatus:
				if ev.IsTerminal() {
					enc.RunFinished(lastUsage)
					return
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
