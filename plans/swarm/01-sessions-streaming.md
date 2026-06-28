# Phase 01 — Server-side sessions + event-sourced streaming / resume

> Decouple runs from the HTTP connection; make them survive disconnect/restart; add exactly-once `?after=<seq>` resume + full-parts history.

Depends on: 00 (identity seam, registry). Required by: 02, 03, 05. Frontend counterpart: [07-frontend-contract.md](07-frontend-contract.md).

## Grounding (current state, verified)

- **Synchronous, connection-bound run.** `internal/agent/stream.go:streamEvents` loops `core.Runner.Run(ctx, "default", threadID, content, ...)` writing directly to the SSE `stream.Encoder`. `ctx = r.Context()` → browser disconnect cancels the run. `userID` hardcoded `"default"`; `handler/threads.go:getUserID` uses `X-User-ID`→`anonymous`. **Inconsistent identity namespaces.**
- **Two persistence layers coexist.** (a) adk `session.Service.AppendEvent` persists every non-partial event (`runner.go:221`) into Valkey LIST `events:{app}:{user}:{session}` (`pkg/session/valkey/session.go:190`) — full `genai.Content` parts. (b) `internal/chat/messages.go:MessageSaver` async-writes **text-only** rows to OpenSearch `messages`. History reads (`handler/threads.go:ThreadsMessagesList`) read the text-only index — the rehydration gap.
- **Heavy heuristics in the stream loop:** parallel-peer grouping (`activeGroup`/`allSeen`/`agentStart`), OutputAgent detection (`worker#taskID` splitting), task-board delta de-dup (`tasks.BoardFromStateDelta`+`Clamp`+`Signature`), `emit_artifact`/`todowrite` interception, thought→reasoning, HITL confirmation interception, usage capture.
- **HITL today** stores `PendingInterrupt` keyed by `threadID` (`hitl.Store`) and returns from the loop; resume (`handler/resume.go`) feeds a synthetic `FunctionResponse` into `Runner.Run`; `runner.go:291-349 findAgentToRun`/`handleUserFunctionCallResponse` re-attaches by function-call ID. **Preserve this.**
- **adk event shape** (`session/session.go:92`, `model/llm.go:42`): `Event{LLMResponse{Content, UsageMetadata, Partial, FinishReason}, ID, Timestamp, InvocationID, Branch, Author, Actions{StateDelta, ArtifactDelta, Escalate, RequestedToolConfirmations}}`. `Partial==true` ⇒ streaming token (NOT persisted by runner). State scopes `app:`/`user:`/`temp:`.
- **Transport seam already abstracted:** `stream.Sink` + `stream.Encoder` are decoupled from `http.ResponseWriter` — the seam we exploit (encoder consumes our EventLog, not the raw adk iterator).

## 1. EventLog port (`internal/eventlog/eventlog.go`)

```go
package eventlog

type SeqEvent struct {
    Seq   int64      // 1-based, monotonic, gap-free per session
    Event AgentEvent
}

type EventLog interface {
    // Append assigns + returns the next seq for sessionID and durably stores ev. Single-writer per session.
    Append(ctx context.Context, sessionID string, ev AgentEvent) (seq int64, err error)
    // Read replays seq>afterSeq, THEN (if follow) streams live until ctx cancel or terminal status. Channel closes at end.
    Read(ctx context.Context, sessionID string, afterSeq int64, follow bool) (<-chan SeqEvent, error)
    // Head returns the latest seq (0 if none).
    Head(ctx context.Context, sessionID string) (int64, error)
}
```

`Read` = the unification of history + live ("replay-then-live"): drain durable backlog `(afterSeq, head]`, then tail new appends with no gap. Redis `XREAD BLOCK` from the last-drained ID registers the tail before draining finishes → nothing lost between drain and tail; the stream itself is the registry (no separate eager-subscribe dance).

### Redis Streams adapter (`internal/eventlog/redis_stream.go`)

- Key per session: `evlog:{appName}:{userID}:{sessionID}`.
- **Seq = dense int64** via atomic Lua: `INCR evlog:seq:{...}` then `XADD evlog:{...} <seq>-0 ...` in one script (atomic ⇒ no races even if a second writer appeared). Dense integers map cleanly to `?after=<seq>` and to Kafka offsets later. (Reject deriving seq from sparse `ms-n` IDs.)
- Replay: `XRANGE key (afterSeqID +COUNT n` in batches.
- Live tail: `XREAD BLOCK <heartbeat-ms> STREAMS key <lastID>`; emit heartbeat `SeqEvent{Seq:-1, Type:heartbeat}` on timeout. Bounded by ctx + observing a terminal `run-status` event.
- Retention: `XADD ... MAXLEN ~ 10000` + `EXPIRE` (reuse 24h default from `pkg/session/valkey`). Redis = hot resumable window; OpenSearch = cold archive. After the OpenSearch flush, shrink the stream TTL (e.g. 1h) so immediate reconnects still hit Redis.
- Ownership: only the run-coordinator goroutine calls `Append`; take Redis lock `evlock:{sessionID}` (SET NX PX) at run start so a second instance can't start a competing writer. Readers unconstrained.

### Kafka later (no rewrite)

`internal/eventlog/kafka.go` implements the same interface: `sessionID`→partition key (order + single-consumer-group), `seq`→offset. `Append`=produce to `agent-events` keyed by sessionID; `Read(afterSeq, follow)`=assign partition, `seek(afterSeq+1)`, consume to HWM then live; `Head`=end offset. Nothing above the port changes — swap the constructor (`EVENTLOG_STORE=redis|kafka`), mirroring `NewSessionService`/`NewHITLStore`.

## 2. Event model (`internal/eventlog/event.go`)

```go
type EventType string
const (
    EvRunStatus EventType = "run-status" // queued|running|awaiting-input|done|error|cancelled
    EvTextDelta="text-delta"; EvReasoningDelta="reasoning-delta"
    EvToolCall="tool-call"; EvToolResult="tool-result"
    EvAgentStep="agent-step"; EvAgentDelta="agent-delta"
    EvTaskList="task-list"; EvArtifact="artifact"; EvUsage="usage"
    EvQuestion="question-asked"; EvHITLResolved="hitl-resolved"
)

type AgentEvent struct {
    V int; Type EventType; Ts int64; InvocationID, Author string
    Step int; IsOutput bool // resolved ONCE at write time
    Text, Kind string; DurationMs int64
    Tool *ToolPayload; Tasks []stream.Task; Artifact map[string]any
    Usage *UsagePayload; Question *QuestionPayload; Status, Err string
}
```

- Versioned (`V`) for forward-compat; reuse `stream.Task` for `task-list` (no third task type).
- **`FromADK(ev *session.Event, prev *mappingState) []AgentEvent`** (`internal/eventlog/from_adk.go`) — the migrated interpretation logic, run **once** in the run goroutine. Mapping table: StateDelta `*:task_board`→`task-list` (after `Clamp`/`Signature`); partial thought→`reasoning-delta`/`agent-delta{reasoning}`; partial text→`text-delta`/`agent-delta{text}`; FunctionCall `adk_request_confirmation`→`question-asked`; other FunctionCall→`tool-call`; FunctionResponse `emit_artifact`→`artifact`(+`tool-result`); `todowrite`→`task-list`(+`tool-result`); other→`tool-result`; UsageMetadata→`usage`; author transition→`agent-step`; iterator err→`run-status{error}`. `IsOutput`/`Step`/`Author` stamped here using `core.OutputAgent`/`SubAgentNames` so **readers never need the Core**.

### Tee, don't tee — recommendation

Introduce `internal/agent/teesession.go:TeeSessionService` implementing `session.Service`, wrapping the real (Valkey/in-mem) service. Delegates everything; in `AppendEvent` it **also** maps to `AgentEvent`s and appends to the EventLog. Why wrap: the runner calls `AppendEvent` for the user message + every non-partial event (`runner.go:171,222,283`) — the canonical, ordered, persisted set — so the durable log is automatically consistent with adk's own memory (one ordering truth), and survives code paths that bypass our loop (resume). **But** partial token deltas never reach `AppendEvent`; so the **run goroutine additionally appends partial `text/reasoning/agent` deltas** from the iterator. Both hit the same `EventLog.Append` ⇒ single seq sequence. Inject the wrapper at `core.go:NewCore`/`NewCoreWithAgent` where `SessionService` is handed to `runner.New`.

## 3. Run coordinator (`internal/run/coordinator.go`)

```go
type Status string // queued|running|awaiting-input|done|error|cancelled
type RunHandle struct { SessionID, UserID, AgentID string; Status Status; HeadSeq int64; StartedAt time.Time; cancel context.CancelFunc }
type Coordinator struct { eventLog eventlog.EventLog; registry *agent.Registry; interrupts hitl.Store; statusStore StatusStore; mu sync.Mutex; active map[string]*runState; logger zerolog.Logger }

func (c *Coordinator) Start(parent context.Context, req RunRequest) (*RunHandle, error)
func (c *Coordinator) Cancel(ctx, sessionID) error
func (c *Coordinator) Resume(ctx, sessionID string, approved bool) (*RunHandle, error)
func (c *Coordinator) Status(ctx, userID, sessionID) (*RunHandle, error)
func (c *Coordinator) List(ctx, userID) ([]*RunHandle, error)
```

**Lifecycle of a run goroutine:**
1. Serialize per session (`active[sessionID]`); if running ⇒ return existing handle (HTTP just attaches via `EventLog.Read`); if `awaiting-input` ⇒ reject new turn. Acquire `evlock:{sessionID}`.
2. **Detached context** `context.WithCancel(context.Background())` — NOT `r.Context()`. Disconnect cancels only the reader. Cancellation is explicit via `Cancel`.
3. Write `run-status{queued}`→`{running}` to EventLog + StatusStore.
4. `SessionManager.GetOrCreate(runCtx, sessionID, userID)` with the **real userID**.
5. User message appended via the runner → `TeeSessionService` → durable event automatically.
6. Loop `for ev := range core.Runner.Run(runCtx, userID, sessionID, content, {SSE})`: `FromADK` + append partial deltas → `EventLog.Append`; maintain `mappingState`.
7. On HITL `question-asked`: store `PendingInterrupt`, append `run-status{awaiting-input}`+`question-asked`, release lock, return goroutine (suspended, not finished). Session stays joinable.
8. On completion: append `usage`, `run-status{done}`, trigger OpenSearch flush, release lock.
9. On error: append `run-status{error}`, flush, release.

**Reconnect/join = attach to the LOG, never the goroutine.** A reconnect is `EventLog.Read(ctx, sessionID, afterSeq, follow=true)` → encoder. Live or finished, the reader gets backlog-then-live (or backlog-then-close). Restart-tolerant for readers.

**StatusStore** (`internal/run/status.go`): Redis hash `runstatus:{app}:{user}:{session}` + per-user set `runs:{app}:{user}` (mirrors existing `sessions:` SET). Backs the sessions-list/status endpoints without scanning the log.

**HITL suspend/resume:** detection unchanged (`toolconfirmation.FunctionCallName` in `from_adk`), but the goroutine returns and status→`awaiting-input`; pending interrupt (Valkey, keyed by session) survives. `POST /v1/agent/resume`→`Coordinator.Resume` rebuilds the confirmation `FunctionResponse` (as `StreamResumeRunFormat` does today) and starts a **new** goroutine continuing the **same** session log (seq continues monotonically).

## 4. HTTP surface

- **`POST /v1/chat/completions`** (modify `handler/chat.go`): resolve `userID := handler.UserID(r)` (fixes `default` vs `anonymous`); same routing; for agent runs call `coordinator.Start` (returns immediately) then attach as a live reader `ch,_ := eventLog.Read(r.Context(), sessionID, 0, true)` pumped through the encoder. `r.Context()` governs only the reader. Preserve `?format=aisdk`, the `x-vercel-ai-ui-message-stream: v1` header, `temporary`/`thread_id`, snake_case body keys. Non-stream path: `Start` then block on `Read(follow=true)` to completion.
- **`GET /v1/sessions/{id}/stream?after=<seq>&format=aisdk`** (new): pure reconnect. `Read(r.Context(), sessionID, after, true)` → encoder. Authz: `userID` owns the session. Emit `start` before replay; suppress duplicate trailing `finish` while the run is live (only emit `finish`/`[DONE]` on a terminal `run-status`).
- **`GET /v1/sessions`** (new): `StatusStore.List(userID)` → `[{sessionId, agentId, status, headSeq, startedAt, title?}]` for the still-running sidebar. Compose with `/v1/threads` by id.
- **`GET /v1/sessions/{id}`** (new): single `RunHandle`.
- **`PumpEventLog(ctx, ch, enc, core)`** (`internal/agent/pump.go`): a thin heuristic-free switch calling the **unchanged** `aisdk`/`openai` encoder methods in the same order ⇒ byte-identical wire contract. `IsOutput`/`Step`/`Author` pre-baked into events.

## 5. Full-parts history (`internal/eventlog/project.go`)

`ProjectMessages(events) []types.ThreadMessage` folds the event sequence into AI-SDK UIMessage parts: user event starts a turn; output text-deltas→one `text` part; reasoning→`reasoning`; tool-call+result→`tool-<name>` part (`state:output-available`, input, output); artifact→`data-artifact`; task-list (last wins)→`data-task-list`; agent-step/agent-delta→`data-agent-step`/`data-agent-delta`. **These shapes are exactly what `aisdk.Encoder` emits live**, so reload renders identically. Persist `parts` on OpenSearch `messages` (field already exists, `index:false`); keep `content` flatten for search/back-compat. Upgrade `MessageSaver.SaveAssistantMessage` to accept `parts`.

## 6. OpenSearch flush (`internal/agent/archive.go`)

At terminal status, async `archiver.Flush(ctx, app, user, session)` reads `Read(after=0, follow=false)` and writes: (a) raw `session_events` index `{app_name, user_id, session_id, seq, type, ts, author, payload(enabled:false)}` — gap-free durable replay; (b) projected `messages` with full parts. Add `IndexSessionEvents` + mapping to `pkg/db/opensearch/indices.go:EnsureIndices`. Composite read (`internal/eventlog/composite.go`): Redis hot → OpenSearch cold for non-follow reads when `Head==0`; live `follow` is Redis-only.

## 7. Refactor `stream.go`

Delete the `streamEvents` heuristic body; it splits into write-side `from_adk.go` (run once in the goroutine) + per-connection heuristic-free `pump.go`. Heuristics simplified: `isOutputAgent`/`baseAgent`→stamped `IsOutput`; parallel-peer grouping→computed once in `mappingState`, emitted as durable `agent-step` events (reconnects replay, not re-derive — kills "second connection disagrees" bugs); board de-dup/usage→emission rules in `from_adk`; HITL→`question-asked` event. What stays in `stream.go`: `setStreamHeaders`, `newEncoder`, format parsing. `stream.Encoder`/`Sink` + both encoders **unchanged**.

## Files

**Add:** `internal/eventlog/{eventlog,event,redis_stream,kafka(stub),composite,from_adk,project}.go`; `internal/run/{coordinator,status}.go`; `internal/agent/{teesession,pump,archive}.go`; `internal/handler/sessions.go`.
**Modify:** `internal/agent/{core,stream,session}.go`; `internal/handler/{chat,resume,threads}.go`; `internal/chat/messages.go`; `internal/server/server.go`; `internal/bootstrap/bootstrap.go`; `pkg/db/opensearch/indices.go`; `internal/config/config.go` (add `EVENTLOG_STORE`).

## Risks / open questions

1. **Seq races** — single-writer lock + atomic Lua. Reject a second `Start` with 409 while `running`.
2. **Exactly-once** — `XADD` exactly-once for writer; readers exactly-once via `seq`+`?after=`. Status is a cache; the log is truth (self-heals on next read).
3. **Restart recovery** — *recommend: replayable history, not auto-resume*. Goroutines die; `evlock` TTL expires; log up to crash is intact + replayable. A startup reconciler scans `runs:{user}` for `running` with expired lock + no live goroutine → mark terminal (`error: interrupted by restart`) + flush. HITL `awaiting-input` survives + resumes. Auto-resume of non-HITL = future (needs turn-level checkpointing).
4. **Token-delta idempotency on replay** — attach with `?after=N` opens a fresh `text-start` and only streams `seq>N`; `Read(afterSeq)` guarantees no double-render.
5. **Backpressure** — bounded per-subscriber channel, drop-oldest **for partial deltas only** (never structural events) + heartbeat; writers only write Redis, readers `XREAD` independently; `MAXLEN ~` caps memory.
6. **adk state scopes** — ignore non-`:task_board` `StateDelta` keys when projecting history; `temp:` discarded; `user:` state is the memory feature (orthogonal).
7. **Identity migration** — `default`→`anonymous` changes the Valkey key namespace; existing dev sessions won't be found. Acceptable; note it. All four key families (`session:`/`events:`/`evlog:`/`runstatus:`) share `{app}:{userID}:{sessionID}`.

## Verification

Start a long run, **close the tab**, reopen → `GET /v1/sessions` shows `running`; `/v1/sessions/{id}/stream?after=0` replays gap-free then continues live. Reload mid-stream preserves reasoning/tool/artifact parts. Restart server mid-run → non-HITL marked terminal but replayable; HITL `awaiting-input` resumes via `/v1/agent/resume`. Verify seq monotonicity in Redis. Unit tests: EventLog seq alloc + replay-then-live; `FromADK`; `ProjectMessages`.
