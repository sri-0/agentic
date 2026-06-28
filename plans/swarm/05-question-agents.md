# Phase 05 — Interactive question agents

> Agents interview the user; rendered in the UI; consistent with opencode's `question` tool.

Depends on: 01 (suspend/resume, `AgentEvent`), 02 (tool plumbing). Frontend counterpart: `agentui/plans/swarm/05-question-ui.md`.

## opencode reference (exact, to mirror)

Input schema (`packages/opencode/src/tool/question.ts` + `packages/schema/src/v1/question.ts`):

```ts
Parameters = { questions: Question[] }
Question   = { question: string, header: string, options: {label,description}[], multiple?: boolean }
// `custom` (free-text "type your own") defaults ON, added automatically.
Answer     = string[]   // array of selected label strings, one array per question
```

Return text: `User has answered your questions: "Q"="label, label", … You can now continue with the user's answers in mind.` Suspend/resume: the tool creates a `Deferred`, **publishes `question.asked`, then blocks on `Deferred.await`** (the whole agent loop parks on the tool call); a client `reply({requestID, answers})` publishes `question.replied` and succeeds the Deferred, unparking the tool. `reject` fails it. The `Request` carries `tool:{messageID, callID}` to tie the prompt back to the originating tool call.

## Design (Go, reuse the HITL machinery)

### `question` tool (`internal/tools/question.go`)

```go
type QuestionItem struct {
    Question string          `json:"question"`
    Header   string          `json:"header"`
    Options  []QuestionOption `json:"options"` // {Label, Description}
    Multiple bool            `json:"multiple,omitempty"`
    Custom   *bool           `json:"custom,omitempty"` // free-text; default true
}
type QuestionArgs struct{ Questions []QuestionItem `json:"questions"` }
```

Implemented exactly like HITL (`toolconfirmation`): the tool **blocks on a future** while the run emits a `question-asked` `AgentEvent` (Phase 01) and the run coordinator suspends the session to `awaiting-input`. A reply unparks the future; the tool returns the formatted answer string (model-facing) as the tool result.

### Wire

- New **`data-question`** part (extend the AI-SDK `ChatDataParts` contract): `{requestId, questions:[{question, header, options, multiple, custom}], toolCallId}`.
- Resolution via the existing **`/v1/agent/resume`** round-trip (`handler/resume.go`), extended to carry **answers** (`{thread_id, request_id, answers: string[][]}`) rather than just approve/deny. The run coordinator's `Resume` path (Phase 01) starts a fresh goroutine continuing the same session log with the answers fed back as the tool result. The HITL `tool-interrupt` flow is the template — generalise it to carry a structured answer payload.
- Emit a `question-replied`/`hitl-resolved` event so reconnecting clients see the resolved state on replay.

## Files

**Add:** `internal/tools/question.go`.
**Modify:** `internal/handler/resume.go` (accept answers), the `AgentEvent`/encoder for `question-asked`/answers (Phase 01 `event.go` + `aisdk`/`openai` encoders), register `question` in `internal/tools/registry.go`. Document the `data-question` contract (keep `agentui/AGENTS.md` + `lib/chat/types.ts` in sync).

## Verification

An agent calls `question(...)` → UI renders the typed question card (options + optional free-text) → answering resumes the run with the answer fed back as the tool result; the model continues coherently. A reconnecting client mid-question sees the pending question on replay; after answering, sees the resolved state. Multi-question and `multiple`/`custom` variants render correctly.
