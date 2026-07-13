# 02 — Run-Framed Event Log & Event-Sourced Resume

**Source commits (agentic main @ 17b1a87):**
- W1 `775a03e` — run-framed event log: multi-turn, concurrent-turn queue, DoS guards, error status, idle eviction
- W2 `cbaf38e` — event-sourced resume via coordinator, question answers through the ADK confirmation payload, per-user session authz, cancel endpoint

**Prerequisite:** plan `01-swarm-baseline.md` is complete — your fork already has the
event log (`EventLog` interface + memory/redis backends), a coordinator skeleton, the
stream encoder, and the tools registry.

**Audience note:** you are porting *intent and architecture* into a diverged fork.
Do not cherry-pick these commits. Read the design below, find your fork's equivalents
of each file, and implement the behavior — including the four fixes in
"Known issues — fix during port" (the upstream code has these bugs; you must ship the
corrected versions).

All `file:line` references below are to the CURRENT state of the upstream repo
(`/Users/sri/code/agentic`), which includes later refinements on top of W1/W2
(turn tracking, terminal flusher, viewed store). Where a cited file contains
post-W2 features your fork lacks, port only what this plan names; the extras are
covered by later plans.

---

## 1. Purpose

Before W1, the event log conflated *session* lifecycle with *run* lifecycle:

- A terminal event (`done`/`error`) closed the session's log readers, so a second
  turn on the same thread could not stream (`Read` returned a dead channel).
- A chat POST arriving while a run was active was silently dropped.
- A negative `?after=` could panic inside the log while holding the session mutex,
  permanently poisoning that session.
- Runner errors were recorded as `StatusDone`; `Cancel` + natural finish could emit
  two terminals.
- Finished sessions were never evicted → unbounded memory.

Before W2, resume (`POST /v1/agent/resume`) streamed the continuation synchronously
to the HTTP response, bypassing the event log entirely (invisible to reconnecting
clients, never recorded), question answers never reached the model (the tool result
was a no-op), and any user could attach to / cancel any session.

The target architecture after this plan:

> **Runs own terminal state; sessions own the log.** One session log carries many
> runs (turns). Readers choose a closure policy (run-attach vs session-follow) in
> the pump — never in the log. Resume is just another run appended to the same log.

---

## 2. Run-framing model

### 2.1 Core concepts

| Concept | Definition |
|---|---|
| **Session (thread)** | One append-only event log, id = threadID. Lives across turns. |
| **Run** | One assistant turn (or one resume continuation). Has `RunID`, `StartSeq` (first seq it writes), exactly one terminal `run-status` event. |
| **Run-attach reader** | Chat POST / resume POST. Reads from `StartSeq-1`, closes at the FIRST terminal it sees (which is its own run's, because it attached after all prior events). |
| **Session-follow reader** | `GET /v1/sessions/{id}/stream?after=N`. Replays then stays live; emits finish framing per terminal but never closes until the client disconnects. |
| **Terminal-close policy** | The **log never closes a follow reader on a terminal** — only ctx cancellation closes it. Closure is decided by the pump mode. |
| **Once-guarded terminal** | `RunHandle.termOnce *sync.Once` — Cancel and natural finish race through the same `terminate()`; first caller wins, exactly one terminal event per run. |
| **Run queue** | A turn arriving while a run is active is enqueued per-session FIFO (`pending map[string][]RunRequest`), drained by the finishing run's goroutine. |
| **Idle eviction** | Background sweeper evicts finished/idle sessions from the coordinator maps and the log (memory backend) after a retention window. Never evicts active or `awaiting-input` sessions. |

### 2.2 Event flow — two turns on one thread

```
 seq:  1        2..8            9           10        11..15         16
      ┌────┐ ┌──────────┐ ┌───────────┐  ┌────┐  ┌──────────┐  ┌───────────┐
      │run-│ │ text/tool│ │run-status │  │run-│  │ text ... │  │run-status │
      │stat│ │  events  │ │  done     │  │stat│  │          │  │  done     │
      │run1│ │  (run1)  │ │  (run1)   │  │run2│  │  (run2)  │  │  (run2)   │
      └────┘ └──────────┘ └───────────┘  └────┘  └──────────┘  └───────────┘
        ▲                        ▲          ▲                        ▲
        │                        │          │                        │
 POST#1 attaches after=0 ────────┘          │                        │
        (closes HERE — its run's terminal)  │                        │
                                            │                        │
 POST#2: Start() → StartSeq=10, attach after=9 ──────────────────────┘
        (sees ONLY run2's events; closes at run2's terminal)

 GET /sessions/{id}/stream?after=0 : replays 1..16, emits finish framing at
 seq 9 AND seq 16, stays open until the client disconnects.
```

### 2.3 Run state machine

```
                       Start()
                          │
          active run? ────┼──── no
              │ yes       ▼
              ▼        RUNNING ──────── runFn error ────────► ERROR*
           QUEUED         │
              │           ├── enc.Interrupted() (question/HITL)
   (drained by            │        ▼
    finish())             │   AWAITING-INPUT ── Resume() ──► RUNNING (new RunID,
              │           │        │                          same session log)
              └──────────►│        └── never swept (sweeper skips it)
                          │
                          ├── runFn ok ─────────────────────► DONE*
                          └── Cancel() ─────────────────────► CANCELLED*

 * hard terminal: exactly one run-status event via termOnce; post-run hooks fire;
   queued turns drain (DONE/ERROR) or are dropped (CANCELLED).
 AWAITING-INPUT is a *suspension*, not a terminal for hooks/viewed/flush purposes,
 but it IS a terminal run-status event in the log (IsTerminal() includes it) so
 run-attach readers close and the UI shows the question.
```

### 2.4 Resume sequence (end-to-end)

```
 model calls question tool (RequireConfirmation: true)
        │
        ▼
 ADK pauses run → encoder emits EvQuestion, sets interrupted=true
        │                      (eventlog_encoder.go:131,141)
        ▼
 coordinator.run(): enc.Interrupted() → outcome AWAITING-INPUT
        │                      (coordinator.go:420-426)
        ▼
 PendingInterrupt{AgentID, ConfirmationCallID, ToolCallID, ToolName, Prompt,
 Details} stored in hitl.Store keyed by threadID (Valkey, TTL 1h — valkey.go:20-26)
        │
        ▼
 UI shows question card. User answers.
        │
        ▼
 POST /v1/agent/resume {thread_id, action, answers[][], text}
        │  handler/resume.go:24-113
        │  - authz gate (see FIX (c) below)
        │  - Interrupts.Get(threadID) → pending
        │  - coord.Resume(core, threadID, userID, pending, approved, answers, text)
        ▼
 Coordinator.Resume (coordinator.go:589-655)
        │  - reject if another user owns session / run already active
        │  - new RunID, StartSeq = Head()+1, append run-status{running}
        │  - goroutine: emit EvHITLResolved (re-surface tool call), then build
        │    ADK confirmation FunctionResponse:
        │      {Name: toolconfirmation.FunctionCallName, ID: pending.ConfirmationCallID,
        │       Response: {"confirmed":bool, "payload":{"answers":[][],"text":""}}}
        │  - streamEvents(confirmContent) → continuation events append to SAME log
        ▼
 ADK unmarshals Response → toolconfirmation.ToolConfirmation{Confirmed, Payload}
        ▼
 questionHandler (tools/question.go:75-77) reads ctx.ToolConfirmation(),
 parseConfirmationPayload extracts [][]string + text (question.go:106-116),
 returns QuestionResult{Answers, Status:"answered", Summary:"User has answered
 your questions: \"Q\"=\"label, label\"..."} — the model sees the Summary and
 continues the turn with the user's choices.
        ▼
 handler attaches the client: StreamSessionAttach(from h.StartSeq-1)
 (resume.go:110-111) → client streams ONLY the continuation; followers see it
 too; terminate() ends awaiting-input state.
```

---

## 3. File inventory

Upstream paths (map to your fork's equivalents):

| File | Role in this plan |
|---|---|
| `internal/agent/coordinator.go` | Run lifecycle: Start/queue/finish/terminate/Resume/Cancel/Status/List, sweeper, confirmation payload builder. THE core file. |
| `internal/agent/pump.go` | `PumpMode` (run-attach vs session-follow) + `PumpEventLog` replay loop; closure policy lives here. |
| `internal/agent/stream_coordinator.go` | `StreamAgentRunBackground` (chat POST → Start + run-attach) and `StreamSessionAttach` (follow). |
| `internal/agent/eventlog_encoder.go` | `interrupted` flag + `Interrupted()`; `EvQuestion` append with structured `QuestionPayload.Questions`. |
| `internal/eventlog/eventlog.go` | Interface contract doc: terminal does NOT close follow readers (lines 18-39). |
| `internal/eventlog/memory.go` | Snapshot+subscribe under one deferred-unlock lock (H1), gap-fill replay, `Evict`/`IdleSince` (lines 88-182, 218-247). |
| `internal/eventlog/redis_stream.go` | Atomic Lua INCR+XADD append, XRANGE replay + XREAD tail, no terminal close (lines 49-166). |
| `internal/eventlog/event.go` | `EvRunStatus`/`EvQuestion`/`EvHITLResolved`/`EvHeartbeat`, `IsTerminal()` (lines 140-145). |
| `internal/handler/sessions.go` | `SessionStream` (authz + `?after=` validation), `SessionCancel`, `SessionStatus`, `SessionsList`. |
| `internal/handler/resume.go` | Resume endpoint: interrupt lookup, authz, coord.Resume, attach. |
| `internal/handler/chat.go:211` | Chat POST calls `StreamAgentRunBackground`. |
| `internal/tools/question.go` | Confirmation-payload decode + model-facing summary. |
| `internal/hitl/hitl.go`, `valkey.go` | `PendingInterrupt` + `Store` (Get/Set/Clear), Valkey impl with 1h TTL. |
| `internal/types/chat.go:91-96` | `ResumeRequest{ThreadID, Action, Answers [][]string, Text}`. |
| `internal/server/server.go:49,56-59` | Routes: `/v1/agent/resume`, `/v1/sessions/{id}`, `/{id}/stream`, `/{id}/cancel`. |
| Tests | `internal/eventlog/reproduction_test.go`, `memory_test.go`, `internal/agent/coordinator_test.go`, `internal/handler/sessions_integration_test.go`, `internal/tools/question_test.go`. |

---

## 4. Step-by-step implementation

### Phase A — W1: run-framed log

**Step A1 — Log never closes on terminal.**
In your memory log's follow loop, remove any `if ev.IsTerminal() { return }`. The
contract (eventlog.go:27-31): follow readers close only on ctx cancellation;
non-follow readers close after backlog. Mirror in the Redis backend's XREAD loop
(redis_stream.go:153-163 — note the comment at 159-162).

**Checkpoint A1:** port/replicate `TestMemoryLog_TerminalDoesNotStickyCloseNewFollow`
and `TestMemoryLogTerminalBeforeFollowStaysLive` — a follow reader opened AFTER a
terminal was appended still receives subsequent events.

**Step A2 — DoS guards (H1).**
- Handler: reject negative/non-integer `?after=` with 400 BEFORE touching the log
  (sessions.go:47-57).
- Log: clamp `afterSeq < 0` to 0 (memory.go:89-91, redis_stream.go:104-106) AND
  make the memory log's snapshot+subscribe critical section use `defer s.mu.Unlock()`
  inside a closure (memory.go:103-118) so a panic can never leave the session mutex
  held.

**Checkpoint A2:** `TestMemoryLog_NegativeAfterSeqNoPanic` — a negative afterSeq
read followed by a normal read both succeed.

**Step A3 — Pump modes.**
Add `PumpMode` (pump.go:12-25) and thread it through `PumpEventLog`:
- `PumpRunAttach`: `return` at first terminal run-status (pump.go:113-116) and at
  `EvQuestion` (pump.go:95-99 — the run is pausing; close the POST stream after
  emitting the interrupt frame).
- `PumpSessionFollow`: on terminal, emit finish framing (`RunFailed(ev.Err)` if
  `StatusError`, else `RunFinished(lastUsage)`), reset `lastUsage`, keep looping
  (pump.go:105-121).

**Step A4 — Coordinator: RunID/StartSeq framing.**
On `RunHandle`, add `RunID` and `StartSeq` (coordinator.go:42-71). In `startLocked`
(coordinator.go:342-386): `StartSeq = Head()+1`, append
`run-status{running, RunID}` before launching the goroutine. All terminal appends
carry `RunID` (coordinator.go:540-542). Chat attaches from `h.StartSeq-1`
(stream_coordinator.go:54-61).

**Step A5 — Run queue (C2), WITH fixes (a)+(b) — see section 6.**
`Start` enqueues when a run is active (coordinator.go:316-333); the finishing run
drains via `finish` → `dequeueLocked` → `startLocked` (coordinator.go:476-487,
564-578). Implement the FIXED version: stable RunID across queue→start, StartSeq
corrected at actual start, single critical section in finish.

**Checkpoint A5:** `TestCoordinator_MultiTurn_SecondTurnStreams` (turn 2 attaches
after turn 1, streams only turn 2's content) and
`TestCoordinator_ConcurrentTurn_Queued` (a POST during an active run is queued,
runs after, and both turns' events land in one log in order). Also
`TestChatHandler_MultiTurn_SecondTurnStreams` at the HTTP layer. Use the `runFn`
seam (coordinator.go:83, `c.runFn = c.defaultRunFunc` at 261) so tests inject a
scripted run without a model — port that seam if your fork lacks it.

**Step A6 — Error status + once-guarded terminal (H5).**
`runFunc` returns `runOutcome{status, err}`; errors map to `StatusError`
(coordinator.go:73-83, 430-470, statusToEvent at 795-806). `terminate` wraps ALL
terminal work in `h.termOnce.Do` (coordinator.go:492-562): status set, removal
from `active`, terminal append, `h.cancel()`, hooks. `Cancel` calls the same
`terminate` with `RunCancelled` and drops the pending queue (coordinator.go:741-753)
— first caller wins, so Cancel followed by the goroutine's natural `finish` yields
exactly one terminal and Cancel's status sticks.

**Checkpoint A6:** `TestCoordinator_ErrorRun_StatusError`,
`TestCoordinator_Cancel_NoDoubleTerminal` (count terminal run-status events in the
log == 1; status stays cancelled). Run with `-race`.

**Step A7 — Idle eviction (M1).**
Log backends gain `Evict(sessionID)` + `IdleSince(sessionID, cutoff) bool`
(memory.go:218-247; Redis relies on key TTL — redis_stream.go:33, the Lua EXPIRE at
lines 56-59 — so its Evict can be a no-op if your fork prefers). The coordinator
detects the capability by interface assertion (coordinator.go:262-267) and runs
`sweepLoop` (coordinator.go:817-829): tick = min(5m, retention), default retention
1h, configurable (`SetSessionRetention`, coordinator.go:160-166). `sweep`
(coordinator.go:846-891) skips: active runs, `awaiting-input` handles (a paused
question must stay resumable — line 858-862), recently-updated handles, and
sessions the log says aren't idle. Eviction removes from `known`/`pending`/`byUser`
and calls `log.Evict` OUTSIDE the coordinator lock. Provide `StopSweeper` for tests
(coordinator.go:894-900).

**Checkpoint A7:** `TestCoordinator_Sweep_RetentionAndHITLGuard` — a done session
past retention is evicted; an awaiting-input session of the same age is not.

### Phase B — W2: event-sourced resume + authz

**Step B1 — ResumeRequest carries answers.**
`types.ResumeRequest` gains `Answers [][]string` and `Text string`
(types/chat.go:91-96). Default action inference: answers or text present →
"approved", else "denied" (resume.go:37-45).

**Step B2 — Coordinator.Resume.**
Port coordinator.go:589-655. Essentials:
- Ownership check against `known` (see fix (c) — coordinator-side check stays, but
  it only *rejects mismatches*; absence is fine).
- Reject if a run is already active for the session (595-598).
- Fresh `RunID`, `StartSeq = Head()+1`, `termOnce = &sync.Once{}` (607 — REQUIRED,
  finish/Cancel route through `terminate`), append `run-status{running}`.
- Goroutine: first append `EvHITLResolved` with the original tool call
  (`pending.ToolCallID/ToolName/Details`) and Kind approved/denied (617-628) so a
  fresh reader sees tool_call before tool_result; then build the confirmation
  content and drive `streamEvents` with it (635-643); map outcome exactly like a
  normal run, including `enc.Interrupted()` → `AWAITING-INPUT` again (a resumed run
  can ask another question) (645-652).

The confirmation payload contract (coordinator.go:755-784):

```go
// coordinator.go:771-784
func buildConfirmationResponse(approved bool, answers [][]string, text string) map[string]any {
    resp := map[string]any{confirmKeyConfirmed: approved}
    if len(answers) > 0 || text != "" {
        payload := map[string]any{}
        if len(answers) > 0 { payload[confirmKeyAnswers] = answers }
        if text != "" { payload[confirmKeyText] = text }
        resp[confirmKeyPayload] = payload
    }
    return resp
}
```

Keys are `"confirmed"` / `"payload"` / `"answers"` / `"text"` — ADK JSON-round-trips
the Response map into `toolconfirmation.ToolConfirmation{Confirmed, Payload}`, so
these names are load-bearing on BOTH sides (encode here, decode in question.go:19-23).

**Step B3 — Question tool decodes the payload.**
Port question.go:75-179: `questionHandler` → `buildQuestionResult(ctx.ToolConfirmation(), args.Questions)`.
Handle: nil/unconfirmed → `{Status:"dismissed"}` with an explicit model-facing
sentence (86-100); JSON-shape tolerance — answers arrive as `[]any` of `[]any` of
string after the ADK round-trip, so `toStringMatrix` must coerce (121-155); the
model-facing `Summary` in opencode format
(`User has answered your questions: "Q"="label, label"...`, 161-179). The tool is
registered with `RequireConfirmation: true` (question.go:60-68).

**Checkpoint B3:** port all four question tests:
`TestQuestionResult_AnswersRoundTripThroughADKPayload` (encode with
`buildConfirmationResponse`, marshal→unmarshal to simulate ADK, decode, assert
answers + summary), `TestQuestionResult_Dismissed`,
`TestQuestionResult_ConfirmedNoAnswers`,
`TestParseConfirmationPayload_ToleratesJSONShapes`.

**Step B4 — Resume handler, WITH fixes (c)+(d) — see section 6.**
Port resume.go:24-113 but with the corrected authz gate and Clear ordering.
Handler shape: decode body → require thread_id → infer action → `Interrupts.Get`
(any core; the store is shared — resume.go:50-58) → authz → resolve core from
`pending.AgentID` → `coord.Resume(...)` → on success attach the client via
`StreamSessionAttach(..., h.StartSeq-1, ...)` (110-111). Resume failure maps to 409.
Thread `*Coordinator` into the handler via your router wiring
(server.go:49).

**Step B5 — Session authz + cancel (H2).**
- `SessionStream`: 404 unless `coord.Status(userID, id)` succeeds (sessions.go:60-67).
  404, not 403 — do not leak existence.
- `SessionCancel`: `POST /v1/sessions/{id}/cancel`, same ownership gate, then
  `coord.Cancel(sessionID)` (sessions.go:76-91; route server.go:58).
- `Coordinator.Start` and `Coordinator.Resume` reject a session last-known to
  belong to another user (coordinator.go:310-315, 591-594). Note the guard is
  tolerant of empty user IDs (unauthenticated/dev mode).
- `Status`/`List` filter by owner (coordinator.go:660-689).

**Checkpoint B5:** `TestSessionStream_CrossUser404` — owner attaches fine (200 +
SSE), other user gets 404 on stream AND cancel.

**Step B6 — Replay fidelity for resumed runs (M3).**
In the pump, `EvHITLResolved` re-emits the originating tool call
(pump.go:100-104); its args may be a structured map (pending.Details), so marshal
if not a string (`hitlResolvedArgs`, pump.go:150-166). Populate
`QuestionPayload.Questions` from the question tool's args when emitting
`EvQuestion` (eventlog_encoder.go:131-145) so the UI gets the structured schema on
replay, not just a prompt string. Ensure `RunFinished` doesn't double-emit usage —
usage comes only from `EvUsage` events; the pump just remembers `lastUsage` for the
finish frame (pump.go:90-92, 110-113).

---

## 5. Key code excerpts

**Run-attach vs session-follow closure (pump.go:105-121):**
```go
case eventlog.EvRunStatus:
    if ev.IsTerminal() {
        if ev.Status == eventlog.StatusError {
            enc.RunFailed(ev.Err)
        } else {
            enc.RunFinished(lastUsage)
        }
        if mode == PumpRunAttach {
            return
        }
        lastUsage = stream.Usage{} // session-follow: keep streaming next run
    }
```

**Chat POST run-attach (stream_coordinator.go:38, 54-61):**
```go
h, err := core.Start(RunRequest{SessionID: sessionID, UserID: userID, ...})
...
after := h.StartSeq - 1
ch, err := core.Log().Read(ctx, sessionID, after, true)
...
PumpEventLog(ctx, ch, enc, PumpRunAttach)
```

**Once-guarded terminal (coordinator.go:492-498, 540-545):**
```go
func (c *Coordinator) terminate(ctx context.Context, sessionID string, h *RunHandle, runID string, outcome runOutcome) {
    h.termOnce.Do(func() {
        c.mu.Lock()
        h.Status = outcome.status
        h.UpdatedAt = c.now()
        delete(c.active, sessionID)
        c.mu.Unlock()
        ...
        _, _ = c.log.Append(ctx, sessionID, eventlog.AgentEvent{
            V: 1, Type: eventlog.EvRunStatus, Status: statusToEvent(outcome.status), Err: outcome.err, RunID: runID,
        })
        if h.cancel != nil { h.cancel() }
        ...
```

**HITL interrupt precedence (coordinator.go:419-425):**
```go
outcome := c.runFn(ctx, req, enc)
if enc.Interrupted() {
    outcome = runOutcome{status: RunAwaitingInput}
}
c.finish(ctx, req, h, runID, outcome)
```

**Atomic Redis append (redis_stream.go:49-61):** Lua `INCR` + `XADD id "<seq>-0"` +
`EXPIRE` in one EVAL, so seq assignment and the stream entry can never diverge; seq
doubles as the `?after=` cursor.

**Memory-log gap-free live tail (memory.go:154-171):** live fan-out is best-effort
(drop on full buffer, memory.go:75-80); before delivering a live event with
`se.Seq > lastSeq+1` the reader back-fills `[lastSeq+1, se.Seq)` from the durable
backlog via `replayRange` — exactly-once, in-order delivery without blocking the
appender.

---

## 6. Known issues — fix during port (do NOT copy upstream verbatim)

These four bugs exist in the upstream code you are porting from. Implement the
fixed versions; each fix is small and localized.

### (a) Queued-turn StartSeq is stale — correct it when the turn actually starts

Upstream (coordinator.go:316-332): a queued handle gets
`StartSeq = Head() + len(pending)`. `Head()` is read WHILE the active turn is still
streaming, so by the time the queued turn runs, the real head is far past that —
the queued handle's StartSeq points into the *previous* run's events. A client that
run-attaches from `qh.StartSeq-1` replays the tail of the wrong turn. Worse,
`startLocked` generates a *new* RunID on dequeue, so the queued handle's RunID is
also a lie.

**Fixed version:**
1. Stamp the RunID at enqueue time and carry it through the queue: add a
   `runID string` field to `RunRequest`; set it in the enqueue branch
   (`qh.RunID`), and have `startLocked` reuse `req.runID` when non-empty instead
   of calling `newRunID()`. Now the queued handle's RunID identifies the eventual
   run.
2. Do NOT attach from a queued handle's StartSeq. In the run-attach path
   (`StreamAgentRunBackground`), when `h.Status == RunQueued`, attach from the
   CURRENT head (`after = Head(sessionID)`) with a RunID filter: skip all events
   until `run-status{running, RunID == h.RunID}` arrives, then stream normally
   until that run's terminal. Implement as a small pre-filter loop before
   `PumpEventLog`, or a `PumpRunAttachFor(runID)` variant that ignores events (and
   especially other runs' terminals) until it sees its run's `running` event.
   Since run-status events already carry RunID (coordinator.go:370, 540-542), the
   log needs no schema change.
3. Still correct `h.StartSeq`/`h.Turn` on the real handle inside `startLocked`
   (upstream already does — the *approximate* values live only on the returned
   queued copy; the comment at coordinator.go:326-327 admits this).

Test to add (upstream lacks it): start run 1 (scripted, slow), enqueue run 2,
attach a client to run 2's handle while run 1 still streams; assert the client
sees NONE of run 1's text and all of run 2's.

### (b) finish() unlock windows can interleave two runs into one session log

Upstream (coordinator.go:476-487): `terminate` removes the session from `active`
under `mu`, releases it, then `finish` re-acquires `mu` to dequeue, releases,
re-acquires to `startLocked`. Two races:
- Between active-removal and the dequeue, a concurrent `Start` sees no active run
  and starts immediately — then `finish` dequeues and ALSO starts the queued turn:
  two goroutines appending to one session log concurrently (violates the
  single-writer contract, eventlog.go:16-17).
- Between the dequeue-unlock and the startLocked-lock, same story.

**Fixed version:** in `finish`, one critical section:
```go
c.mu.Lock()
if _, stillActive := c.active[sessionID]; !stillActive {
    if next, ok := c.dequeueLocked(sessionID); ok {
        c.startLocked(next)   // launches goroutine; safe under mu
    }
}
c.mu.Unlock()
```
And harden `Start`: enqueue not only when a run is active, but ALSO when
`len(c.pending[sessionID]) > 0` — otherwise a new turn can jump the queue in the
same window. (`startLocked` only takes `mu`-protected actions plus a non-blocking
`go`; holding `mu` across it is safe — upstream already calls it with `mu` held.)

Test to add: N goroutines POSTing turns to one session concurrently with a
scripted runFn; assert the log contains N `running` and N terminal events, and
that no two runs' event ranges interleave (each `running(RunID)`..`terminal(RunID)`
span is contiguous per RunID).

### (c) Resume authz 404s after idle sweep — gate on the interrupt store, not in-memory Status

Upstream (resume.go:63-69) rejects resume with 404 when
`coord.Status(userID, threadID)` fails. But `Status` reads the coordinator's
in-memory `known` map, which the idle sweeper prunes. The sweeper never evicts
`awaiting-input` handles (coordinator.go:858-862) — but a process restart, or any
future eviction path, loses the map while the Valkey `PendingInterrupt` (TTL 1h,
hitl/valkey.go:20-26) is still perfectly valid. Result: a legitimate resume is
permanently 404'd even though everything needed to resume exists.

**Fixed version:** make the interrupt store the authz source of truth:
1. Add `UserID string` to `hitl.PendingInterrupt` (hitl/hitl.go) and stamp it where
   the interrupt is stored (the encoder/stream path that writes it when the run
   pauses — find your fork's `Interrupts.Set` call site).
2. In the handler: after `Interrupts.Get`, require
   `pending.UserID == "" || pending.UserID == userID` → else 404.
3. Keep the coordinator check only as a *mismatch* rejection: if
   `coord.Status(userID, threadID)` returns a handle owned by someone else that's
   a 404, but a coordinator MISS must NOT block the resume. (Simplest: drop the
   handler-side `coord.Status` gate entirely and rely on (2) plus
   `Coordinator.Resume`'s own `known`-map ownership check at coordinator.go:591-594,
   which already tolerates absence.)

Test to add: store an interrupt, evict/clear the coordinator maps (or use a fresh
coordinator over the same log + store, simulating restart), POST resume as the
owner → expect the continuation to run, not 404; as another user → 404.

### (d) Interrupts.Clear before Resume can fail — clear only after success

Upstream (resume.go:92-93) clears the pending interrupt BEFORE calling
`coord.Resume`. If Resume then fails — "run already active" (coordinator.go:595-598),
ownership rejection, anything → the handler returns 409 but the interrupt is gone:
the run is stuck in `awaiting-input` forever with no way to answer it.

**Fixed version:** reorder — call `coord.Resume` first; on error return 409/404
with the interrupt intact; on success, `Interrupts.Clear(threadID)` immediately
after `Resume` returns (before attaching the stream). The double-fire race the
upstream comment worries about is closed by `Coordinator.Resume` itself: the
second concurrent resume finds a run already active for the session and errors
(coordinator.go:595-598). If your fork wants belt-and-braces, implement
claim-semantics in the store (`GETDEL` in Valkey) and re-`Set` on Resume failure —
but clear-after-success alone is correct.

Test to add: with an active run for the session, POST resume → 409 AND the
interrupt still `Get`s; cancel the run, POST resume again → succeeds and interrupt
is cleared.

---

## 7. Fork-adaptation notes

- **Names will differ.** Match on responsibility, not filename: the thing your 01
  port calls "coordinator" gains the queue/terminate/Resume/sweeper; your pump/replay
  function gains the mode parameter. If your fork's chat handler still streams
  synchronously, `StreamAgentRunBackground` (stream_coordinator.go:33-62) is the
  template for switching it to Start+attach.
- **RunHandle extras:** upstream's current `RunHandle` also carries `Turn`, `Viewed`,
  `core`, `messages`, and the coordinator has `TerminalFlusher`/`ViewedStore`/
  `TaskBoardStore`/start-hooks. Those are LATER work — skip them unless your fork
  already has equivalents. The W1/W2 essentials are: `SessionID, UserID, AgentID,
  RunID, Status, StartSeq, StartedAt, UpdatedAt, cancel, termOnce`.
- **`runFn` seam:** keep it (coordinator.go:83, 98, 261). Every coordinator test
  depends on injecting a scripted run; without it you need a live model to test.
- **ADK coupling:** the confirmation flow assumes ADK's
  `toolconfirmation.FunctionCallName` + JSON round-trip into
  `ToolConfirmation{Confirmed, Payload}` (see upstream's citation of
  `request_confirmation_processor.go` in the W2 commit message). If your fork
  pins a different ADK version, verify that processor still unmarshals the
  Response map the same way — the `"confirmed"`/`"payload"` key names come from
  ADK, not from this codebase.
- **Interrupt store:** if your fork stores pending interrupts in-process, move to
  the shared store (Valkey) BEFORE fix (c) matters — the handler reads it via "any
  core" (resume.go:50-56) precisely because it's shared.
- **Redis vs memory:** upstream's Redis eviction is TTL-based (24h stream TTL,
  redis_stream.go:33); only the memory backend implements `Evict`/`IdleSince`. The
  coordinator's interface-assertion wiring (coordinator.go:262-267) means backends
  without eviction just skip the log half of the sweep — keep that tolerance.
- **Heartbeats:** both backends emit `SeqEvent{Seq: -1, EvHeartbeat}` on idle
  (memory.go:152-175, redis_stream.go:144-151); the pump ignores them
  (pump.go:47-48) and seq bookkeeping must skip `Seq < 0`. If your fork's SSE
  path lacks keep-alives, port this too or proxies will kill idle awaiting-input
  streams.
- **404-not-403 policy:** every ownership failure in this plan is a 404. Keep that
  consistent in the fork; a mixed 403/404 surface leaks session existence.

---

## 8. Verification

### 8.1 Unit tests to port/replicate (all race-clean: `go test -race ./...`)

From `internal/eventlog/reproduction_test.go` + `memory_test.go`:
- `TestMemoryLog_TerminalDoesNotStickyCloseNewFollow`
- `TestMemoryLog_NegativeAfterSeqNoPanic`
- `TestMemoryLogTerminalBeforeFollowStaysLive`
- `TestMemoryLogResumeAfterSeq`, `TestMemoryLogReplayThenLive` (if 01 didn't already)

From `internal/agent/coordinator_test.go`:
- `TestCoordinator_MultiTurn_SecondTurnStreams`
- `TestCoordinator_ConcurrentTurn_Queued`
- `TestCoordinator_ErrorRun_StatusError`
- `TestCoordinator_Cancel_NoDoubleTerminal`
- `TestCoordinator_Sweep_RetentionAndHITLGuard`

From `internal/handler/sessions_integration_test.go`:
- `TestChatHandler_MultiTurn_SecondTurnStreams`
- `TestSessionStream_CrossUser404`

From `internal/tools/question_test.go`:
- `TestQuestionResult_AnswersRoundTripThroughADKPayload`
- `TestQuestionResult_Dismissed`
- `TestQuestionResult_ConfirmedNoAnswers`
- `TestParseConfirmationPayload_ToleratesJSONShapes`

Plus the resume-handler test (upstream: `TestResumeHandler` in
`internal/handler/chat_test.go`, updated in W2 to drive the coordinator path and
assert the re-surfaced tool call), and the NEW tests for fixes (a), (b), (c), (d)
described in section 6.

### 8.2 curl smoke sequence (resume + cancel)

Prereq: server running with Valkey (or memory fallback), an agent whose toolset
includes the `question` tool.

```bash
BASE=http://localhost:8080
TID="smoke-$(date +%s)"

# 1. Trigger a question interrupt (prompt engineered to force the tool).
curl -N -s "$BASE/v1/chat/completions" -H 'Content-Type: application/json' -d '{
  "model": "default", "stream": true, "thread_id": "'"$TID"'",
  "messages": [{"role":"user","content":"Use the question tool to ask me whether to proceed with plan A or plan B before doing anything."}]
}'
# EXPECT: SSE stream ends with a tool-interrupt frame (question card payload);
# the HTTP stream CLOSES (run-attach closes on EvQuestion).

# 2. Session status shows the suspension.
curl -s "$BASE/v1/sessions/$TID"
# EXPECT: {"status":"awaiting-input", "run_id":"run-...", "start_seq":N, ...}

# 3. Answer via resume; note answers is a list-of-lists (one list per question).
curl -N -s "$BASE/v1/agent/resume?format=aisdk" -H 'Content-Type: application/json' -d '{
  "thread_id": "'"$TID"'", "action": "approved",
  "answers": [["Plan A"]], "text": "prefer the cheaper option"
}'
# EXPECT: SSE continuation — first the re-surfaced question tool_call, then its
# tool_result containing "answers":[["Plan A"]] and the summary string, then
# model text that actually references Plan A, then finish framing.

# 4. Replay proves it was event-sourced (fresh reader, full history).
curl -N -s "$BASE/v1/sessions/$TID/stream?after=0" & sleep 2; kill %1
# EXPECT: turn 1 events, question, hitl-resolved + tool result, continuation,
# TWO finish framings, stream stays open (session-follow) until killed.

# 5. Resume with nothing pending → 404.
curl -s -o /dev/null -w '%{http_code}\n' "$BASE/v1/agent/resume" \
  -H 'Content-Type: application/json' -d '{"thread_id":"'"$TID"'","action":"approved"}'
# EXPECT: 404 (interrupt was cleared AFTER the successful resume — fix (d)).

# 6. Cancel flow: start a long turn, cancel it mid-stream.
TID2="smoke-cancel-$(date +%s)"
curl -N -s "$BASE/v1/chat/completions" -H 'Content-Type: application/json' -d '{
  "model":"default","stream":true,"thread_id":"'"$TID2"'",
  "messages":[{"role":"user","content":"Write a very long essay about event sourcing."}]
}' & sleep 2
curl -s -X POST "$BASE/v1/sessions/$TID2/cancel"
# EXPECT: {"session_id":"...","cancelled":true,...}; the streaming curl gets its
# finish frame and closes; GET /v1/sessions/$TID2 shows "cancelled" (not "done").

# 7. Authz: repeat 2/4/6 with a different user's auth header → 404 on
#    /v1/sessions/{id}, /stream, /cancel, and /v1/agent/resume.

# 8. DoS guard:
curl -s -o /dev/null -w '%{http_code}\n' "$BASE/v1/sessions/$TID/stream?after=-1"
# EXPECT: 400, and the session still streams fine afterwards (mutex not poisoned).
```

### 8.3 Multi-turn smoke

POST two sequential turns with the same `thread_id`: the second stream must contain
ONLY the second answer (no replay of turn 1) and must not hang. Then POST a third
turn WHILE the second is still streaming: it must not be dropped — after turn 2's
finish, turn 3's events appear (verify via the session-follow reader), and with
fix (a) the third POST's own stream shows only turn 3.

---

*Next in series: 03 (question/HITL UI payloads + swarm decision surfaces) builds on
the `EvQuestion`/`EvHITLResolved` events defined here.*
