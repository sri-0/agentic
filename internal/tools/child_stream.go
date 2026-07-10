package tools

import (
	"context"
	"encoding/json"

	"agentic/internal/eventlog"

	"google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
)

// childStreamState carries the mutable accumulation while translating a child's
// SSE event stream into parent-log events. finalText accumulates the last full
// (non-partial) assistant message so multi-part answers are joined rather than
// truncated (fixes the M2 Reset()-per-part bug). hadPartial tracks whether the
// final text already streamed as deltas so we don't double-emit it.
type childStreamState struct {
	finalText  string // last complete assistant text (joined parts)
	hadPartial bool   // partial text streamed since the last non-partial event
	blocked    bool   // child emitted an adk_request_confirmation (question/HITL)
}

// translateChildEvent maps one child ADK session.Event to zero or more parent-log
// AgentEvents attributed to the child. label is "subagentType#shortID"; childID
// is the child's session id (surfaced as SessionID + SubagentType metadata so
// the UI can link the sub-agent card to its own session). step is the parent-log
// step index for this child instance.
//
// It is a pure function (no I/O) so it can be unit-tested by driving it with fake
// child events. The returned events are ready to Append to the PARENT log.
func translateChildEvent(ev *session.Event, label, subagentType, childID string, step int, st *childStreamState) []eventlog.AgentEvent {
	if ev == nil || ev.Content == nil {
		return nil
	}
	var out []eventlog.AgentEvent
	add := func(e eventlog.AgentEvent) {
		e.Author = label
		e.SubagentType = subagentType
		e.SessionID = childID
		e.Step = step
		out = append(out, e)
	}

	// Partial event = streaming token → agent-delta.
	if ev.Partial {
		for _, p := range ev.Content.Parts {
			if p.Text == "" {
				continue
			}
			if p.Thought {
				add(eventlog.AgentEvent{Type: eventlog.EvAgentDelta, Kind: eventlog.KindReasoning, Text: p.Text})
			} else {
				add(eventlog.AgentEvent{Type: eventlog.EvAgentDelta, Kind: eventlog.KindText, Text: p.Text})
				st.hadPartial = true
			}
		}
		return out
	}

	// Non-partial event: process parts. A fresh full assistant message resets the
	// accumulated final text (each non-partial content event is one message), then
	// joins its text parts.
	msgText := ""
	for _, p := range ev.Content.Parts {
		if p.Text != "" && p.Thought {
			add(eventlog.AgentEvent{Type: eventlog.EvAgentDelta, Kind: eventlog.KindReasoning, Text: p.Text})
			continue
		}
		if p.Text != "" {
			msgText += p.Text
			// Only surface a text delta if it didn't already stream as partials.
			if !st.hadPartial {
				add(eventlog.AgentEvent{Type: eventlog.EvAgentDelta, Kind: eventlog.KindText, Text: p.Text})
			}
		}

		// A child that calls question/HITL emits an adk_request_confirmation; the
		// child has no resume path here, so flag it (the caller ends the child with
		// a note rather than deadlocking silently).
		if fc := p.FunctionCall; fc != nil && fc.Name == toolconfirmation.FunctionCallName {
			st.blocked = true
			continue
		}
		if fc := p.FunctionCall; fc != nil {
			args, _ := json.Marshal(fc.Args)
			add(eventlog.AgentEvent{Type: eventlog.EvToolCall,
				Tool: &eventlog.ToolPayload{ID: fc.ID, Name: fc.Name, Args: string(args)}})
		}
		if fr := p.FunctionResponse; fr != nil {
			if fr.Name == toolconfirmation.FunctionCallName {
				continue
			}
			res, _ := json.Marshal(fr.Response)
			add(eventlog.AgentEvent{Type: eventlog.EvToolResult,
				Tool: &eventlog.ToolPayload{ID: fr.ID, Name: fr.Name, Result: string(res)}})
		}
	}
	if msgText != "" {
		st.finalText = msgText // last full message wins; parts already joined
	}
	st.hadPartial = false
	return out
}

// childLogSink appends child-attributed events into the parent session's log.
// eventlog.Append is safe for concurrent writers, so a background child running
// on its own goroutine can append to the same parent log as the foreground run.
type childLogSink struct {
	ctx      context.Context
	log      eventlog.EventLog
	parentID string
}

func (s *childLogSink) append(ev eventlog.AgentEvent) {
	if s.log == nil {
		return
	}
	ev.V = 1
	_, _ = s.log.Append(s.ctx, s.parentID, ev)
}
