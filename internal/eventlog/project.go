package eventlog

import "encoding/json"

// ProjectedMessage is one AI-SDK UIMessage-shaped message folded from a session's
// event sequence. Role is "user" or "assistant"; Parts is the ordered AI-SDK
// parts array (text / reasoning / dynamic-tool / data-* parts) that the frontend
// renders. Content is the flattened assistant/user text, kept for search and
// text-only back-compat. It is persisted verbatim on the OpenSearch `messages`
// index (Parts on the `parts` field, Content on `content`) so a reload rehydrates
// full messages that render identically to the live stream.
type ProjectedMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Parts   []Part `json:"parts"`

	// Metadata folded from the EvMetadata event the encoder appends on run finish
	// (mirrors the live `message-metadata` frame). These persist on the archive
	// message doc and rehydrate the footer (model · agent · duration) on reload.
	Model      string `json:"model,omitempty"`
	AgentID    string `json:"agentId,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`

	// Turn is the 0-based index of this assistant message within the session's
	// event log (Nth flushed assistant turn). It is STABLE across re-projections
	// of a growing log, so the archive can derive a deterministic OpenSearch _id
	// ({sessionID}:{Turn}:{Role}) that upserts each message in place instead of
	// appending a duplicate on every re-flush.
	Turn int `json:"-"`
	// TsMillis is the unix-ms timestamp of the first event that opened this
	// assistant message. It is stable across re-flushes, so the archive can use it
	// as a deterministic created_at (ordering stays correct even when an earlier
	// turn is re-written after a later user message was saved).
	TsMillis int64 `json:"-"`
}

// Part is one AI-SDK UIMessage part. Only the fields relevant to a given Type are
// populated (omitempty keeps the JSON compact and identical to the live wire
// shapes emitted by internal/stream/aisdk). Tool parts use the AI-SDK
// "dynamic-tool" shape (the live encoder marks tool calls dynamic:true), so the
// UI's dynamic-tool renderer lights up on reload exactly as it does live.
type Part struct {
	Type string `json:"type"`

	// text / reasoning parts.
	Text string `json:"text,omitempty"`

	// reasoning part timing (opencode model): unix-ms of the first and last
	// reasoning delta. StartedMs/EndedMs round-trip through the archive so the
	// reload can show the REAL "Thought for N seconds" instead of a static label.
	StartedMs int64 `json:"startedMs,omitempty"`
	EndedMs   int64 `json:"endedMs,omitempty"`

	// dynamic-tool part.
	ToolName   string `json:"toolName,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	State      string `json:"state,omitempty"`
	Input      any    `json:"input,omitempty"`
	Output     any    `json:"output,omitempty"`

	// data-* parts (data-artifact / data-task-list / data-agent-step / data-agent-delta).
	ID   string `json:"id,omitempty"`
	Data any    `json:"data,omitempty"`
}

// Part type discriminators mirroring the AI-SDK UIMessage parts array.
const (
	PartText          = "text"
	PartReasoning     = "reasoning"
	PartDynamicTool   = "dynamic-tool"
	PartArtifact      = "data-artifact"
	PartTaskList      = "data-task-list"
	PartAgentStep     = "data-agent-step"
	PartAgentDelta    = "data-agent-delta"
	PartToolInterrupt = "data-tool-interrupt"
)

// ProjectMessages folds a session's ordered event sequence into AI-SDK
// UIMessage-shaped messages. It is a PURE function of the event slice (no I/O),
// so it is exhaustively unit-tested.
//
// Folding rules (the projected parts match what internal/stream/aisdk emits live,
// so a reload renders identically):
//   - a user event (EvTextDelta / EvHITLResolved carrying user text is not
//     produced here; user turns are recorded by the coordinator via
//     SaveUserMessage). Within this projector a run's output builds ONE assistant
//     message: consecutive output text-deltas coalesce into a single `text` part;
//     reasoning-deltas coalesce into a `reasoning` part.
//   - a tool-call + its matching tool-result (by tool call id) become one
//     `dynamic-tool` part with state "output-available", input, and output. An
//     unresolved tool-call stays "input-available".
//   - an artifact becomes a `data-artifact` part.
//   - a task-list snapshot becomes a `data-task-list` part; the LAST snapshot wins
//     (a single stable part id "tasks", updated in place).
//   - sub-agent agent-step / agent-delta events (Author != "", not IsOutput)
//     become `data-agent-step` / `data-agent-delta` parts.
//
// A run terminal (EvRunStatus done/error/cancelled) flushes the current assistant
// message; awaiting-input does NOT flush (the run continues after resume).
func ProjectMessages(events []AgentEvent) []ProjectedMessage {
	p := &projector{taskParts: map[string]int{}}
	for _, ev := range events {
		p.fold(ev)
	}
	p.flush()
	return p.messages
}

// NextTurn returns the Turn index the next assistant message flushed from this
// event sequence will receive. It folds the events through the SAME projector
// ProjectMessages uses — the single source of truth for turn numbering — but
// WITHOUT the trailing flush: a log whose last run is still open (mid-run /
// awaiting-input) has not materialised that message yet, so NextTurn reports
// the OPEN message's turn and a resumed continuation keeps the same identity.
// This is what lets the live encoder stamp {session}:{turn}:assistant on the
// start frame and have it match the archiver's deterministic doc id exactly.
func NextTurn(events []AgentEvent) int {
	p := &projector{taskParts: map[string]int{}}
	for _, ev := range events {
		p.fold(ev)
	}
	return p.turn
}

type projector struct {
	messages []ProjectedMessage

	// current assistant message under construction.
	open         bool
	parts        []Part
	content      string
	openTs       int64          // ts (unix ms) of the first event that opened this message
	model        string         // EvMetadata: model id for this assistant turn
	agentID      string         // EvMetadata: agent id for this assistant turn
	durationMs   int64          // EvMetadata: wall-clock elapsed for this turn
	turn         int            // 0-based index of the NEXT flushed assistant message
	textIdx      int            // index into parts of the open text part, -1 if none
	rsnIdx       int            // index into parts of the open reasoning part, -1 if none
	toolIdx      map[string]int // tool call id -> parts index
	taskParts    map[string]int // task-list id -> parts index (last-wins in place)
	interruptIdx map[string]int // tool call id -> data-tool-interrupt parts index
}

func (p *projector) ensureOpen(ts int64) {
	if p.open {
		return
	}
	p.open = true
	p.parts = nil
	p.content = ""
	p.openTs = ts
	p.model = ""
	p.agentID = ""
	p.durationMs = 0
	p.textIdx = -1
	p.rsnIdx = -1
	p.toolIdx = map[string]int{}
	p.taskParts = map[string]int{}
	p.interruptIdx = map[string]int{}
}

func (p *projector) fold(ev AgentEvent) {
	switch ev.Type {
	case EvTextDelta:
		if !ev.IsOutput {
			return
		}
		p.ensureOpen(ev.Ts)
		// reasoning part closes when output text starts (mirrors the live encoder).
		p.rsnIdx = -1
		if p.textIdx < 0 {
			p.textIdx = len(p.parts)
			p.parts = append(p.parts, Part{Type: PartText})
		}
		p.parts[p.textIdx].Text += ev.Text
		p.content += ev.Text

	case EvReasoningDelta:
		if !ev.IsOutput {
			return
		}
		p.ensureOpen(ev.Ts)
		p.textIdx = -1
		if p.rsnIdx < 0 {
			p.rsnIdx = len(p.parts)
			p.parts = append(p.parts, Part{Type: PartReasoning, StartedMs: ev.Ts})
		}
		p.parts[p.rsnIdx].Text += ev.Text
		// Capture the FIRST delta's ts as start (only if unset) and the LAST as end,
		// so the persisted reasoning part carries the real thinking window.
		if p.parts[p.rsnIdx].StartedMs == 0 {
			p.parts[p.rsnIdx].StartedMs = ev.Ts
		}
		if ev.Ts > 0 {
			p.parts[p.rsnIdx].EndedMs = ev.Ts
		}

	case EvToolCall:
		p.ensureOpen(ev.Ts)
		id := toolCallID(ev)
		part := Part{
			Type:       PartDynamicTool,
			ToolName:   toolCallName(ev),
			ToolCallID: id,
			State:      "input-available",
			Input:      normalizeArgs(ev),
		}
		p.textIdx = -1
		p.rsnIdx = -1
		p.toolIdx[id] = len(p.parts)
		p.parts = append(p.parts, part)

	case EvToolResult:
		p.ensureOpen(ev.Ts)
		id := toolCallID(ev)
		if idx, ok := p.toolIdx[id]; ok {
			p.parts[idx].State = "output-available"
			p.parts[idx].Output = toolResultValue(ev)
		} else {
			// result without a preceding call — surface a standalone part.
			p.parts = append(p.parts, Part{
				Type: PartDynamicTool, ToolName: toolCallName(ev), ToolCallID: id,
				State: "output-available", Output: toolResultValue(ev),
			})
		}

	case EvArtifact:
		p.ensureOpen(ev.Ts)
		id := artifactID(ev.Artifact)
		p.parts = append(p.parts, Part{Type: PartArtifact, ID: id, Data: artifactData(ev.Artifact)})

	case EvTaskList:
		p.ensureOpen(ev.Ts)
		data := map[string]any{"tasks": taskItems(ev.Tasks)}
		if idx, ok := p.taskParts["tasks"]; ok {
			p.parts[idx].Data = data // last snapshot wins, updated in place
		} else {
			p.taskParts["tasks"] = len(p.parts)
			p.parts = append(p.parts, Part{Type: PartTaskList, ID: "tasks", Data: data})
		}

	case EvAgentStep:
		if ev.Author == "" {
			return
		}
		p.ensureOpen(ev.Ts)
		status := "started"
		if ev.Kind == KindDone {
			status = "done"
		}
		data := map[string]any{"agent": ev.Author, "step": ev.Step, "status": status}
		if ev.Duration != 0 {
			data["durationMs"] = ev.Duration
		}
		p.parts = append(p.parts, Part{Type: PartAgentStep, ID: agentStepID(ev), Data: data})

	case EvAgentDelta:
		if ev.Author == "" {
			return
		}
		p.ensureOpen(ev.Ts)
		kind := KindText
		if ev.Kind == KindReasoning {
			kind = KindReasoning
		}
		p.parts = append(p.parts, Part{Type: PartAgentDelta, ID: agentStepID(ev),
			Data: map[string]any{"agent": ev.Author, "step": ev.Step, "kind": kind, "delta": ev.Text}})

	case EvQuestion:
		// HITL / interactive question: fold the SAME two parts the live encoder
		// emits (aisdk.Encoder.ToolInterrupt) — the interrupted tool call as a
		// dynamic-tool part and the data-tool-interrupt side channel that drives
		// the question card — so a session-aware reload of an awaiting-input run
		// still shows the pending question.
		if ev.Question == nil {
			return
		}
		p.ensureOpen(ev.Ts)
		p.textIdx = -1
		p.rsnIdx = -1
		q := ev.Question
		var details any
		var threadID string
		if q.Details != nil {
			details = q.Details["details"]
			threadID, _ = q.Details["thread_id"].(string)
		}
		if _, ok := p.toolIdx[q.ToolCallID]; !ok {
			p.toolIdx[q.ToolCallID] = len(p.parts)
			p.parts = append(p.parts, Part{
				Type: PartDynamicTool, ToolName: q.ToolName, ToolCallID: q.ToolCallID,
				State: "input-available", Input: details,
			})
		}
		p.interruptIdx[q.ToolCallID] = len(p.parts)
		p.parts = append(p.parts, Part{Type: PartToolInterrupt, ID: q.ToolCallID, Data: map[string]any{
			"toolCallId": q.ToolCallID,
			"toolName":   q.ToolName,
			"prompt":     q.Prompt,
			"details":    details,
			"threadId":   threadID,
		}})

	case EvHITLResolved:
		// The question/confirmation was answered and the run resumed. Mark the
		// interrupt part resolved (so a reload never re-raises an already-answered
		// question card) and re-surface the originating tool call, mirroring the
		// live pump (which emits a real ToolCall here).
		p.ensureOpen(ev.Ts)
		id := toolCallID(ev)
		if idx, ok := p.interruptIdx[id]; ok {
			if data, ok := p.parts[idx].Data.(map[string]any); ok {
				resolved := KindApproved
				if ev.Kind == KindDenied {
					resolved = KindDenied
				}
				data["resolved"] = resolved
			}
		}
		if idx, ok := p.toolIdx[id]; ok {
			p.parts[idx].Input = normalizeArgs(ev)
		} else {
			p.textIdx = -1
			p.rsnIdx = -1
			p.toolIdx[id] = len(p.parts)
			p.parts = append(p.parts, Part{
				Type: PartDynamicTool, ToolName: toolCallName(ev), ToolCallID: id,
				State: "input-available", Input: normalizeArgs(ev),
			})
		}

	case EvMetadata:
		// The encoder appends this on run finish (model / agent id / elapsed),
		// mirroring the live `message-metadata` frame. Stamp it onto the currently
		// open assistant message so flush persists it (footer rehydration on reload).
		p.ensureOpen(ev.Ts)
		if ev.Model != "" {
			p.model = ev.Model
		}
		if ev.Author != "" {
			p.agentID = ev.Author
		}
		if ev.Duration != 0 {
			p.durationMs = ev.Duration
		}

	case EvRunStatus:
		// A hard terminal flushes the assistant message; awaiting-input keeps it
		// open (the run continues after the HITL/question resume).
		if ev.Status == StatusDone || ev.Status == StatusError || ev.Status == StatusCancelled {
			p.flush()
		}
	}
}

func (p *projector) flush() {
	if !p.open {
		return
	}
	if len(p.parts) > 0 {
		p.messages = append(p.messages, ProjectedMessage{
			Role:       "assistant",
			Content:    p.content,
			Parts:      p.parts,
			Turn:       p.turn,
			TsMillis:   p.openTs,
			Model:      p.model,
			AgentID:    p.agentID,
			DurationMs: p.durationMs,
		})
		// Advance the turn index only for a materialised assistant message, so the
		// Nth assistant turn always maps to the same Turn across re-projections.
		p.turn++
	}
	p.open = false
	p.parts = nil
	p.content = ""
	p.openTs = 0
	p.model = ""
	p.agentID = ""
	p.durationMs = 0
}

// ── field extraction helpers ─────────────────────────────────────────────────

func toolCallID(ev AgentEvent) string {
	if ev.Tool != nil {
		return ev.Tool.ID
	}
	return ""
}
func toolCallName(ev AgentEvent) string {
	if ev.Tool != nil {
		return ev.Tool.Name
	}
	return ""
}
func toolResultValue(ev AgentEvent) any {
	if ev.Tool != nil {
		return ev.Tool.Result
	}
	return nil
}

// normalizeArgs mirrors the live encoder's parseJSON: tool args are stored as a
// pre-marshalled JSON string, so the projected `input` is the parsed object (the
// UI expects an object, not a string) — falling back to the raw string on error.
func normalizeArgs(ev AgentEvent) any {
	if ev.Tool == nil || ev.Tool.Args == nil {
		return map[string]any{}
	}
	if s, ok := ev.Tool.Args.(string); ok {
		if s == "" {
			return map[string]any{}
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return s
		}
		return v
	}
	return ev.Tool.Args
}

func artifactID(m map[string]any) string {
	if m != nil {
		if s, ok := m["id"].(string); ok && s != "" {
			return s
		}
	}
	return "artifact"
}

// artifactData mirrors aisdk.Encoder.Artifact's data shape.
func artifactData(m map[string]any) map[string]any {
	kind, _ := m["kind"].(string)
	if kind == "" {
		kind = "markdown"
	}
	data := map[string]any{
		"id":      artifactID(m),
		"title":   strOf(m, "title"),
		"kind":    kind,
		"content": strOf(m, "content"),
	}
	if lang := strOf(m, "language"); lang != "" {
		data["language"] = lang
	}
	// File artifacts (e.g. office documents) carry a URL/filename/mime instead
	// of inline content.
	if url := strOf(m, "url"); url != "" {
		data["url"] = url
	}
	if fn := strOf(m, "filename"); fn != "" {
		data["filename"] = fn
	}
	if mime := strOf(m, "mime"); mime != "" {
		data["mime"] = mime
	}
	return data
}

func strOf(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func taskItems(items []TaskItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, t := range items {
		row := map[string]any{"id": t.ID, "title": t.Title, "status": t.Status}
		if t.Priority != "" {
			row["priority"] = t.Priority
		}
		if t.Agent != "" {
			row["agent"] = t.Agent
		}
		out = append(out, row)
	}
	return out
}

func agentStepID(ev AgentEvent) string {
	// mirror aisdk: "<agent>-<step>" (data-agent-step) — a stable per-step id.
	return ev.Author + "-" + itoa(ev.Step)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
