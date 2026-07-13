# 04 — Durability, Full-Parts History, Memory Glue

> **Series**: code-migration plan, document 4. Prerequisites: docs 01 (event log +
> coordinator core), 02, 03. Source repo state: merged `main` @ `17b1a87`.
>
> **Audience**: an agent porting these changes into a **diverged fork**. Do not
> blind-copy files. The fork has its own local changes — read each fork file
> first, implement the *behaviour* described here, keep the fork's naming and
> local adaptations, and port the tests so the behaviour is pinned. Where this
> doc says "Known issue — fix during port", implement the **fixed** version in
> the fork, not the bug-for-bug copy.

## Commits covered

| Commit | What it did |
|---|---|
| `d295abe` | W4: Redis Lua atomic append, OpenSearch cold archive (`session_events`), pure event→message projection (`ProjectMessages`), PostRunHooks (archive / memory / title / compaction-trigger), coordinated non-stream path |
| `a4651ba` | `messages` index: map `parts` as `object, enabled:false` (was `text` → 400 `mapper_parsing_exception`, assistant rows silently never persisted) |
| `f7b1017` | Reload dedup (deterministic `{session}:{turn}:{role}` doc ids), persist RAW user text (pre-memory-recall), task status normalization |
| `082e617` | Thread READ ownership: `ThreadsGet` gate + `user_id` filter on messages list |
| `6ff2811` | Cap outbound `max_tokens`; surface model errors as run errors (not silent `done`) |
| `88e738d` | Dedupe model ids in `config/default/models.yaml` (React duplicate-key errors) |
| `7687842` | Persist `user_id` on user messages (reload regression after the ownership filter) |
| `d89753c` | `agents.yaml` prompt guidance: in-conversation recall from history, memory tools only for cross-conversation facts |
| `888ab8c` (PR #13) | Memory repair: kNN filter **inside** the `knn` clause (Lucene engine), write dedup at score ≥ 0.97, junk-fact rejection with sentinel errors |

---

## 1. Purpose and architecture

Before W4 a turn lived only in the hot Redis stream (24h TTL) and the assistant
message was persisted as a text-only row by an ad-hoc saver. W4 makes every turn
**durable and reconstructible**:

1. **Atomic append** — the per-session seq counter INCR and the stream XADD
   happen in ONE server-side Lua step. A crash or a competing writer can never
   open a seq gap or emit an out-of-order entry. Seq is the resume cursor
   (`?after=<seq>`) and must be dense and monotonic.
2. **Hot/cold split** — Redis Streams is the *hot* window (live tail + recent
   replay, TTL 24h, `MAXLEN ~10000`). OpenSearch `session_events` is the *cold*
   archive, flushed on run terminal. `CompositeLog` serves non-follow history
   from hot while it exists, cold after TTL expiry. Follow (live tail) is
   always hot-only.
3. **One source of message truth** — `eventlog.ProjectMessages` is a **pure
   function** `[]AgentEvent → []ProjectedMessage`. The archiver, the
   session-aware threads API, and the turn-numbering (`NextTurn`) all fold
   through the SAME projector, so the live stream, a mid-run reload, and the
   post-run archive all render identically. Nothing else may construct
   assistant message rows.
4. **PostRunHooks** — archive flush, memory extraction, auto-title, and the
   compaction trigger are registered side-effects fired async (each in its own
   panic-guarded goroutine) exactly once per hard terminal. The run lifecycle
   knows nothing about them.
5. **Coordinated non-stream path** — `stream:false` turns run THROUGH the
   coordinator too, so they are recorded/resumable/archivable, then the handler
   collects final text from the event log and answers in OpenAI
   ChatCompletion shape.
6. **Memory glue** — pre-run kNN recall injected into the model context;
   post-run extraction writes durable facts. PR #13 fixed the kNN query shape
   (see §6 — this is the single most subtle bug in this doc) and added write
   dedup + junk rejection.

## 2. Data-flow diagram

```
 user turn (POST /v1/chat/completions)
      │
      │  handler/chat.go: capture RawUserText, injectMemoryRecall() prepends
      │  recalled facts to the LAST message content (model sees augmented text)
      ▼
 ┌─────────────┐   Start(RunRequest{RawUserText,...})   ┌──────────────────────┐
 │ Coordinator  │───────────────────────────────────────▶│ run goroutine        │
 │ (turn = Next │   fires RunStartHooks (early title)    │ eventLogEncoder      │
 │  Turn(log))  │                                        └──────────┬───────────┘
 └─────────────┘                                                    │ Append()
      │ SaveUserMessage(raw text, turn)                             ▼
      │ → messages doc id {session}:{turn}:user          ┌──────────────────────┐
      ▼                                                  │ Redis Stream (HOT)   │
 ┌─────────────┐                                         │ evlog:{app}:{sid}    │
 │ OpenSearch  │                                         │ Lua: INCR+XADD+EXPIRE│
 │  threads /  │                                         │ id "<seq>-0", TTL 24h│
 │  messages   │                                         └──────────┬───────────┘
 └─────────────┘                                                    │
      ▲              live SSE / non-stream readers  ◀───────────────┤ XREAD BLOCK
      │                                                             │
      │  run TERMINAL (done/error/cancelled)                        │ XRANGE
      │  ┌───────────────────────────────────────────────┐          │
      │  │ terminate():                                  │          ▼
      │  │  1. SYNC FlushWaitRefresh (Task C, ≤10s)      │   ┌─────────────┐
      │  │  2. append terminal run-status event          │   │ readAll()   │
      │  │  3. fire PostRunHooks async (panic-guarded):  │   └──────┬──────┘
      │  │     ArchiveHook · MemoryExtractorHook ·       │          │
      │  │     TitleHook · CompactionTriggerHook         │          ▼
      │  └───────────────────────────────────────────────┘   ProjectMessages()
      │                                                       (PURE projector)
      │        ┌──────────────────────────────────────────────────┬─────────┐
      │        ▼                                                  ▼         │
      │  session_events (COLD archive,                     messages index   │
      │  one doc per seq, payload not indexed)             {sid}:{turn}:role│
      │        │                                                  │         │
      │        │ ColdStore.ReadHistory (after hot TTL)            │         │
      │        ▼                                                  ▼         │
      │  ┌────────────────┐   non-follow reads    GET /v1/threads/{id}/messages
      └──│ CompositeLog   │◀─────────────────     (ownership-gated; active runs
         │ hot ▸ cold     │                        merge live ProjectMessages
         └────────────────┘                        over archived rows)
```

## 3. File inventory

Every path is relative to repo root. Read the fork's counterpart before touching it.

| File | Role |
|---|---|
| `internal/eventlog/redis_stream.go` | Hot log. Lua atomic append (`appendScript` :49-61), XRANGE replay + XREAD tail, heartbeats, `Head` via seq counter |
| `internal/eventlog/composite.go` | `ColdStore` interface + `CompositeLog` hot▸cold cutover for non-follow reads and `Head` |
| `internal/eventlog/project.go` | `ProjectedMessage` / `Part` types, `ProjectMessages` (pure fold), `NextTurn` |
| `internal/eventlog/project_test.go` | **13 tests** pinning the projector. Port them verbatim (adapt names only) |
| `internal/eventlog/memory.go` | In-memory log used by contract tests; grew ~15s heartbeats on idle follow readers |
| `internal/eventlog/taskstate.go` | `TaskBoardStore` (Redis + in-memory) per-session current task board |
| `internal/agent/archive.go` | `Archiver`: flush hot log → `session_events` + projected `messages`; implements `ColdStore.ReadHistory` |
| `internal/agent/hooks.go` | `ArchiveHook`, `MemoryExtractorHook`, `TitleHook`, `TitleStartHook`, `CompactionTriggerHook` |
| `internal/agent/coordinator.go` | `PostRunHook` type + `AddPostRunHook`/`AddRunStartHook`/`fireHooks` (:200-246), terminal flush Task C (:492-562), `RawUserText`/`turn` on `RunRequest` (:276-298) |
| `internal/agent/nonstream_coordinator.go` | `NonStreamAgentRunCoordinated` — stream:false through the coordinator |
| `internal/chat/messages.go` | `SaveUserMessage` (deterministic `{tid}:{turn}:user` id, thread-doc upsert). Also `SaveAssistantMessageWithParts` — **dead code, do NOT port** (§7d) |
| `internal/handler/threads.go` | Threads/messages REST; ownership gates (:140-149, :249-262); session-aware live fold (:234-323) |
| `internal/handler/chat.go` | `injectMemoryRecall` (:162-171, :252-289), RawUserText capture, max_tokens cap + run-error surfacing (6ff2811) |
| `pkg/memory/memory.go` | Memory service: `Add` (junk + dedup guards), `Search` (kNN with nested filter), sentinel errors |
| `pkg/memory/tools.go` | `save_memory` / `search_memories` agent tools |
| `pkg/db/opensearch/indices.go` | Index names + mappings (`MessagesMapping`, `MemoriesMapping`, `SessionEventsMapping`) + `EnsureIndices` |
| `internal/bootstrap/bootstrap.go` | Wiring: archiver, composite log, terminal flusher, hook registration (:280-295, :414-422) |
| `config/default/agents.yaml` | Prompt guidance (d89753c, 888ab8c): history vs memory-tool recall |
| `config/default/models.yaml` | Model-id dedupe (88e738d) — check the fork's model list for the same duplicated ids rather than replaying the diff |

---

## 4. Implementation steps (with checkpoints)

Work in this order; each phase compiles and tests green before the next.

### Phase 1 — Atomic Lua append (redis_stream.go)

Replace any INCR-then-XADD two-step in the fork with the single EVAL. The
entire point is that seq assignment and the stream write are indivisible.

`internal/eventlog/redis_stream.go:49-61`:

```lua
local seq = redis.call('INCR', KEYS[2])
if tonumber(ARGV[2]) > 0 then
  redis.call('XADD', KEYS[1], 'MAXLEN', '~', ARGV[2], seq .. '-0', 'd', ARGV[1])
else
  redis.call('XADD', KEYS[1], seq .. '-0', 'd', ARGV[1])
end
if tonumber(ARGV[3]) > 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[3])
  redis.call('EXPIRE', KEYS[2], ARGV[3])
end
return seq
```

Key facts to preserve:

- `KEYS[1]` = `evlog:{app}:{session}` stream, `KEYS[2]` = `evlog:seq:{app}:{session}`
  counter. `ARGV` = event JSON, maxlen (0 disables trim), TTL seconds.
- Entry id is `"<seq>-0"` so the stream id IS the seq — `seqFromID` just parses
  the ms half. `Head()` reads the counter key (GET, nil→0), not XINFO.
- Defaults (`:33`): TTL 24h, block 15000ms, `maxLen` 10000 with `MAXLEN ~`
  (approximate trim). See §7b before you keep 10000.
- `Append` (`:79-99`) stamps `ev.Ts = now` when zero *before* marshalling, so
  projections have a stable per-event time.
- `appendKeys`/`appendArgs` are split out so command composition is
  unit-testable without a live Valkey — keep that seam and its tests
  (`redis_stream_test.go`).
- Follow readers: on XREAD timeout emit a heartbeat `SeqEvent{Seq:-1}`
  (`:145-152`); terminal run-status events do **not** close a follow reader
  (sessions hold many turns — closure policy lives in the pump). The in-memory
  log (`memory.go`) must heartbeat identically or SSE via the memory backend
  gets proxy-idle-killed.

**Checkpoint 1**: `go test ./internal/eventlog/` green (contract tests +
`redis_stream_test.go` command-composition tests). If the fork has a live
Valkey harness: append 100 events concurrently from 2 goroutines, assert seqs
are exactly 1..200 with no gaps or duplicate stream ids.

### Phase 2 — Cold archive + CompositeLog

1. Add `IndexSessionEvents = "session_events"` and `SessionEventsMapping`
   (`pkg/db/opensearch/indices.go:150-163`): `seq: long`, `session_id/type/author:
   keyword`, `ts: long`, and critically `"payload": {"type":"object","enabled":false}`
   — the raw AgentEvent JSON is replayed, never queried by inner field. Wire it
   into the fork's `EnsureIndices` equivalent.
2. Port `internal/eventlog/composite.go`. Contract:
   - `Read(follow=true)` → hot only, always.
   - `Read(follow=false)` → hot if `Head() > 0`, else cold `ReadHistory`
     filtered by `afterSeq` (seq is stable across hot and cold). Cold errors
     degrade to the hot store's empty read — never a hard failure (but see §7g:
     log the degradation).
   - `Head()` prefers hot; on 0 it reports the cold archive's last seq
     (implemented today by fetching the whole backlog — fix per §7c).
   - `Evict`/`IdleSince` delegate to the hot store only.
3. Port `internal/agent/archiver` (`archive.go`). Contract:
   - `FlushAsync` → detached goroutine, 30s timeout, warn-log on error.
   - `flush(waitRefresh)` reads the FULL hot log (`readAll`, skipping
     heartbeat seqs < 0), writes (a) one `session_events` doc per seq with id
     `{sessionID}:{seq}` and (b) `ProjectMessages` output to `messages` with
     deterministic id `{sessionID}:{turn}:{role}` (`archive.go:140`) —
     re-flushes of a growing log UPSERT in place, never duplicate. **Implement
     the bulk version per §7a, not the per-doc loop.**
   - Message doc fields (`archive.go:118-136`): `thread_id`, `user_id`, `role`,
     `content` (flattened text), `parts` (raw JSON of the parts array),
     `created_at` derived from `TsMillis` (stable across re-flushes so
     ordering survives), plus optional `model`, `agent_id`, `duration_ms`.
   - `FlushWaitRefresh` = same with `refresh=wait_for` on message docs (used by
     the synchronous terminal flush so a reload right after `done` can search
     the fresh row).
   - `ReadHistory` implements `ColdStore`: term query on `session_id`, sort
     `seq asc` (§7b applies to its `"size": 10000`).
   - Every method no-ops on a nil OpenSearch client (local dev without the
     store must keep working).
4. Bootstrap wiring (`internal/bootstrap/bootstrap.go:280-295`):

```go
archiver := agent.NewArchiver(osClient, eventLog, cfg.AppName, logger)
compositeLog := eventlog.NewCompositeLog(eventLog, archiver)
// ... coordinator constructed over eventLog; readers go through compositeLog
runCoordinator.SetTerminalFlusher(agent.TerminalFlusherFunc(archiver.FlushWaitRefresh), cfg.AppName)
```

**Checkpoint 2**: `composite_test.go` ports green (memory hot + fake cold:
hot-preferred while Head>0, cold replay after eviction, afterSeq honoured
against cold, follow never touches cold).

### Phase 3 — ProjectMessages: THE single source of message truth

Port `internal/eventlog/project.go` in full. This is the heart of the doc.
**Insist on porting `project_test.go` — all 13 tests — before wiring anything
that consumes the projector.** They pin the exact folding semantics; a fork
that "roughly" reimplements them will diverge from its own live encoder and
reload rendering breaks in ways nobody notices until a demo:

```
TestProjectMessages_CoalescesTextDeltas         TestProjectMessages_ReasoningPart
TestProjectMessages_ToolCallResultPaired        TestProjectMessages_UnresolvedToolCall
TestProjectMessages_ArtifactPart                TestProjectMessages_TaskListLastWins
TestProjectMessages_SubAgentParts               TestProjectMessages_MultiTurn
TestProjectMessages_AwaitingInputKeepsMessageOpen
TestProjectMessages_SubAgentTextNotFlattened    TestProjectMessages_Empty
TestProjectMessages_JSONShape                   TestProjectMessages_TurnIndexStableAcrossReflush
```

Folding rules (`project.go:100-107` doc-comment is normative; `:162-353` is the
fold):

- Pure function, no I/O. One run's output = ONE assistant message.
- Consecutive output `text-delta`s coalesce into one `text` part; reasoning
  deltas into one `reasoning` part carrying `StartedMs`/`EndedMs` (first/last
  delta ts — powers the real "Thought for N s" on reload). Output text closes
  the open reasoning part and vice versa (mirrors the live aisdk encoder).
- Non-output deltas (`IsOutput == false`, i.e. sub-agent internal text) are
  NOT flattened into the assistant text.
- Tool-call + matching tool-result (by call id) → one `dynamic-tool` part,
  state `output-available`; unresolved call stays `input-available`; a result
  with no preceding call surfaces standalone. Tool args are a pre-marshalled
  JSON string → `normalizeArgs` parses to an object (`:408-423`), the UI
  expects an object.
- Artifact → `data-artifact`; task-list snapshot → single `data-task-list`
  part id `"tasks"`, LAST snapshot wins in place; sub-agent step/delta →
  `data-agent-step` / `data-agent-delta` keyed `"<agent>-<step>"`.
- `EvQuestion` folds the interrupted tool call + a `data-tool-interrupt` part
  (pending question survives reload); `EvHITLResolved` marks it resolved
  (approved/denied) so an answered question never re-raises.
- `EvMetadata` stamps model/agent/duration onto the OPEN message.
- **Flush rule** (`:347-353`): `run-status` done/error/cancelled flushes;
  `awaiting-input` keeps the message OPEN (a resumed continuation keeps the
  same identity). `Turn` increments only on a materialised (non-empty) flush,
  which is what makes `{session}:{turn}:{role}` stable across re-projections
  (`TestProjectMessages_TurnIndexStableAcrossReflush`).
- `NextTurn` (`:117-123`) = same fold WITHOUT the trailing flush. The
  coordinator uses it to stamp `Turn` on the handle so the live start-frame
  `messageId` equals the archiver's doc id by construction
  (`coordinator.go:394-413`).

Fork note: if the fork's event type names or `AgentEvent` fields diverged,
adapt the fold's `case` arms — but the OUTPUT shapes (part `type` strings,
field names, `dynamic-tool` state values) must match the fork's live AI-SDK
encoder, whatever that emits. The invariant is projector ≡ live encoder, not
projector ≡ this repo.

**Checkpoint 3**: all 13 projector tests green in the fork.

### Phase 4 — PostRunHooks + terminal flush

1. Coordinator mechanism (`coordinator.go`): `PostRunHook func(PostRunInfo)`
   (`:186-200`), `AddPostRunHook`/`AddRunStartHook` (bootstrap-time only, not
   concurrency-safe), `fireHooks` runs each hook in its own goroutine with a
   `recover()` guard (`:234-246`) — one panicking hook must never kill the
   process or starve the others. `firePostRun` filters `awaiting-input`
   (suspension, not terminal) (`:226-231`).
2. Terminal ordering (`terminate`, `coordinator.go:492-562`) — port this
   ordering EXACTLY, it closes a UX race:
   1. status stamped, handle removed from `active`;
   2. **synchronous** `termFlusher.Flush` (= `FlushWaitRefresh`, 10s bound,
      skipped on awaiting-input) — the full-parts assistant doc is
      written+searchable BEFORE anyone can observe completion;
   3. terminal `run-status` event appended (readers unblock here);
   4. async post-run hooks fire.
3. Hooks (`internal/agent/hooks.go`) — each no-ops when a dependency is nil:
   - `ArchiveHook` (`:22-29`) → `archiver.FlushAsync`. Idempotent with step 2
     via the deterministic doc ids; unlike step 2 it also captures the
     terminal event itself into `session_events`.
   - `MemoryExtractorHook` (`:39-65`) → runs the `memory_extractor` internal
     agent over `lastUserText(info.Messages)`, splits bullet lines
     (`splitFacts`), writes each via `mem.Add`. Fires on `RunDone` only.
     **This is the only hook that writes memories.** Expect and tolerate
     `ErrDuplicateMemory` / `ErrJunkMemory` from `Add` — they are successful
     no-ops, warn-log anything else.
   - `TitleStartHook` (`:107-132`, run-START hook) → early async title from the
     first user message; `TitleHook` (`:76-93`, post-run) is the idempotent
     fallback — both funnel through `generateAndSetTitle`, which re-checks
     `titleUnset` just before writing so a user rename is never clobbered.
   - `CompactionTriggerHook` (`:175-203`) → reads the last `EvUsage` from the
     log and LOGS when `used >= context_window - reserved`. Detection only;
     `CompactionService.CompactFull` is the named plug-in point.
4. Registration (`bootstrap.go:414-422`): Archive, MemoryExtractor, Title,
   CompactionTrigger as post-run; TitleStart as run-start.
5. User-message persistence on the run path (`defaultRunFunc`,
   `coordinator.go:430-470`): `SaveUserMessage(raw text, turn)` with the
   deterministic `{tid}:{turn}:user` id and `user_id` on the doc (7687842 —
   without it the ownership filter hides user rows on reload). `RawUserText`
   (f7b1017) persists what the user TYPED, while the model receives the
   memory-recall-augmented `last.Content`. `streamEvents` gets a **nil saver**
   — the archive path owns the assistant row (§7d).

**Checkpoint 4**: `hooks_test.go` + `coordinator_test.go` ports green; a stub
run through the coordinator produces exactly one terminal event, fires hooks
once, and a hook that panics only logs.

### Phase 5 — Coordinated non-stream path

Port `internal/agent/nonstream_coordinator.go` (whole file, ~95 lines, quoted
behaviour): `coord.Start(...)` → attach a follow reader at `h.StartSeq-1` (sees
only THIS turn) → coalesce `IsOutput` text-deltas → stop on terminal
run-status, mapping error→`"error"`, awaiting-input→`"tool_calls"` → respond
with standard ChatCompletion JSON + `thread_id`. 10-minute read bound; the run
itself continues detached on client disconnect. Route the fork's stream:false
branch here instead of any direct/bypass runner.

Also from `6ff2811` (handler/chat.go + encoders): clamp the outbound
`max_tokens` to the provider cap, and surface upstream model errors as run
ERRORS (terminal `run-status: error` + visible error frame), not a silent
empty `done`. Check the fork's gateway for an equivalent seam before porting
the exact diff.

**Checkpoint 5**: `curl -s localhost:PORT/v1/chat/completions -d '{"stream":false,...}'`
returns content AND the same turn appears in `session_events` + `messages`
after the terminal.

### Phase 6 — Threads API: ownership + session-aware reload

`internal/handler/threads.go`:

- `ThreadsGet` gate (`:140-149`): owner mismatch → **404, not 403** (existence
  must not leak). Note the `thread.UserID != ""` escape hatch — §7e.
- `ThreadsMessagesList` (`:249-262`): query scopes by BOTH `thread_id` AND
  `user_id`.
- Session-aware fold (`:285-318`): if `coord.Status(userID, threadID)` reports
  an ACTIVE run (running/awaiting-input/queued), fold the live log via
  `projectLiveMessages` (which is just `ProjectMessages` over a full non-follow
  read through the CompositeLog), `mergeLiveMessages` upserts by deterministic
  id over archived rows, and the response is wrapped
  `{"data":[...], "live":{"head_seq","turn","status"}}` so the client attaches
  the stream at exactly `?after=head_seq`. Terminal-transition window (run done
  but archive not yet searchable, `:306-316`): fold once more, plain-array
  response. `coord.Status` doubles as the ownership gate for the fold — an
  unowned session never folds.
- **Port the ownership gate to the mutation endpoints too — §7f. In this repo
  they are still ungated; the fork must not inherit that.**

**Checkpoint 6**: two users, one thread id: owner reloads full history
mid-run; the other user gets 404 on GET thread, empty on messages, and (after
your §7f fix) 404 on update/delete/message-create.

### Phase 7 — Memory: kNN fix, dedup, junk rejection (PR #13)

Port `pkg/memory/memory.go` behaviour (see §6 for the kNN query shape):

- `Add` (`:73-109`): trim → `isJunkContent` guard → embed once → `findDuplicate`
  → index. Sentinel errors `ErrDuplicateMemory` (returns the EXISTING id) and
  `ErrJunkMemory` — callers (`MemoryExtractorHook`, the `save_memory` tool,
  `handler/memories.go`) treat both as no-op success.
- `dedupScoreThreshold = 0.97` (`:26`) — Lucene `cosinesimil` maps cosine to
  `(1+cos)/2 ∈ [0,1]`; 0.97 catches re-phrasings without merging distinct facts.
- `isJunkContent` (`:115-131`): empty, or a `key: value` / `key - value` pair
  whose value matches `(?i)^(none|unknown|n/?a|null|nil|undefined)$`, or a bare
  placeholder. Blocks extractor junk like `Work: NONE` that otherwise wins
  targeted recall queries.
- `findDuplicate` (`:136-188`): kNN k=1 with the nested filter, score ≥
  threshold → duplicate; fallback exact normalized-text match (lowercase,
  collapsed whitespace) for the no-embedding path.
- Prompt guidance (`d89753c` + 888ab8c's `agents.yaml` lines): agents recall
  same-conversation facts from HISTORY; memory tools are only for
  cross-conversation persistence. Port the wording into the fork's agent
  prompts — this moved Gemini multi-turn recall from ~25% to ~75%.

**Checkpoint 7**: §9 curl checks pass against a live stack.

---

## 5. Key code excerpts

Deterministic identity — the thread that ties live/reload/archive together:

```go
// internal/agent/archive.go:140
docID := fmt.Sprintf("%s:%d:%s", sessionID, m.Turn, m.Role)
// internal/chat/messages.go:37
docID = fmt.Sprintf("%s:%d:user", threadID, turn)
// internal/handler/threads.go:287
assistantID := fmt.Sprintf("%s:%d:assistant", threadID, h.Turn)
```

Panic-guarded hook dispatch (`internal/agent/coordinator.go:234-246`):

```go
func (c *Coordinator) fireHooks(hooks []PostRunHook, info PostRunInfo) {
    for _, h := range hooks {
        h := h
        go func() {
            defer func() {
                if r := recover(); r != nil {
                    c.logger.Error().Interface("panic", r).Str("session", info.SessionID).Msg("run hook panicked")
                }
            }()
            h(info)
        }()
    }
}
```

Hot/cold cutover (`internal/eventlog/composite.go:37-51`):

```go
if follow || c.cold == nil {
    return c.EventLog.Read(ctx, sessionID, afterSeq, follow)
}
head, err := c.EventLog.Head(ctx, sessionID)
if err == nil && head > 0 {
    return c.EventLog.Read(ctx, sessionID, afterSeq, false)   // hot wins while alive
}
events, err := c.cold.ReadHistory(ctx, sessionID)             // TTL expired → cold replay
```

Terminal flush ordering (`internal/agent/coordinator.go:513-541`, abridged):

```go
if c.termFlusher != nil && outcome.status != RunAwaitingInput {
    fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
    if err := c.termFlusher.Flush(fctx, c.app, h.UserID, sessionID); err != nil { /* warn; async hook retries */ }
    fcancel()
}
// terminal event appended AFTER the flush so no reader observes `done`
// before the full-parts doc exists
_, _ = c.log.Append(ctx, sessionID, eventlog.AgentEvent{ ... Status: statusToEvent(outcome.status) ... })
```

## 6. The kNN bug (PR #13) — read before touching memory code

**Symptom**: semantic memory search returned only lexical-fallback results;
combined recall queries missed facts that were definitely stored.

**Cause**: with the **Lucene** engine (`MemoriesMapping`: `engine: lucene`,
`space_type: cosinesimil`), a `knn` query nested under `bool.must` with the
(app_name, user_id) scoping as a SIBLING `bool.filter` returns **zero hits** —
silently. The broken shape looked reasonable and threw no error:

```jsonc
// BROKEN on Lucene — 0 hits, silent degrade to text fallback
{ "query": { "bool": {
    "must":   [ { "knn": { "vector": { "vector": [...], "k": 5 } } } ],
    "filter": [ { "term": { "app_name": "agentic" } },
                { "term": { "user_id": "u1" } } ] } } }
```

**Fix**: the filter must nest **inside** the knn clause (this also makes it a
true pre-filter — k nearest among the user's own docs, not global-k-then-filter).
Correct shape, used by both `Search` (`pkg/memory/memory.go:243-263`) and
`findDuplicate` (`:143-161`):

```json
{
  "size": 5,
  "query": {
    "knn": {
      "vector": {
        "vector": [0.0123, "... query embedding, 1024 dims ..."],
        "k": 5,
        "filter": {
          "bool": {
            "filter": [
              { "term": { "app_name": "agentic" } },
              { "term": { "user_id": "u1" } }
            ]
          }
        }
      }
    }
  }
}
```

(The outer `"vector"` key is the FIELD NAME in the memories mapping; the inner
one is the query vector. If the fork named its knn_vector field differently,
substitute the outer key.)

Fork check: if the fork uses `engine: nmslib` or `faiss`, filter semantics
differ (nmslib doesn't support Lucene-style efficient pre-filtering) — verify
against the fork's actual engine, don't assume. If the fork uses the Lucene
engine anywhere else (e.g. RAG over `embeddings`, which has the identical
mapping), audit those queries for the same sibling-filter bug.

## 7. Known issues — fix during port

These exist in the source repo TODAY. The fork should implement the **fixed**
versions; do not replicate the bugs for fidelity.

**(a) Archive flush: one HTTP call per event, errors swallowed.**
`internal/agent/archive.go:84-101` loops `IndexDocument` once per event doc
(a 5k-event session = 5k sequential HTTP round-trips inside a 30s async
timeout / 10s terminal-flush budget) and each failure is only warn-logged —
`flush` **returns nil even if every single write failed**, so callers believe
the session is archived and the hot TTL later erases the only copy. Fix: use
the OpenSearch **`_bulk` API** (chunk ~500-1000 docs per request), parse the
bulk response's per-item errors, and aggregate into a returned error when any
item failed (the terminal flusher and FlushAsync both log it; the async hook's
retry semantics then mean something). Keep the message-doc loop's
`refresh=wait_for` behaviour by putting `?refresh=wait_for` on the final bulk
request when `waitRefresh`.

**(b) >10k-event sessions lose history.** Three interacting limits:
`RedisStreamLog.maxLen = 10000` with `MAXLEN ~` trim (`redis_stream.go:33,52`),
`ReadHistory`'s hardcoded `"size": 10000` (`archive.go:157`), and the composite
cutover preferring hot whenever `Head > 0` (`composite.go:41-44`). Head is the
INCR counter — it keeps counting past the trim — so once a session exceeds
~10k events the hot XRANGE silently starts at the trimmed tail while the
composite still routes non-follow reads to hot: early events vanish from
replay AND from the next archive flush (`readAll` reads the trimmed hot log).
The cold read caps at 10k docs on top. Fix (pick one, document the choice in
the fork): (i) paginate `ReadHistory` with `search_after` on `seq` until
exhausted, and make the composite MERGE cold(1..trim-start) + hot(tail) when
`head > maxLen`; or (ii) explicitly document ~10k events as the hard per-
session ceiling and make the archiver flush incrementally (per-terminal since
last flushed seq) so trims can't lose unarchived events. Option (ii) is less
code; option (i) is correct for long-lived sessions.

**(c) Composite `Head` on an expired hot store fetches the entire cold
backlog for one number** (`composite.go:100-104` calls `ReadHistory` and takes
`events[len-1].Seq`). Fix: add a cheap cold-head — a `session_id`-filtered
search with `"size": 1, "sort": [{"seq": "desc"}]` (or a `max` aggregation) —
as a `ColdStore` method or type-asserted optional interface.

**(d) `SaveAssistantMessageWithParts` is dead code and a schema-divergent
second writer — do NOT port it.** (`internal/chat/messages.go:80-114`.) The
coordinator passes a nil saver to `streamEvents` precisely so the archiver is
the only assistant-row writer; this method survives with random UUID doc ids
(breaks the deterministic-upsert scheme → duplicate rows on re-flush), no
`user_id` (rows invisible through the ownership filter), and no
`agent_id`/`duration_ms`. Any fork call site that still uses it (or
`SaveAssistantMessage`) should be rerouted to the archive path and the methods
deleted.

**(e) Legacy threads with empty `user_id` are readable (and per §7f writable)
by anyone.** Every gate has the escape hatch `thread.UserID != "" && ...`
(`threads.go:143`; same idea at `coordinator.go:312,721`). Decide a backfill
in the fork: enumerate `threads` docs with missing/empty `user_id`, assign the
owner where derivable (e.g. from their messages' `user_id` or the
single-tenant default), and then REMOVE the empty-user escape from the gates
(empty owner → 404). If the fork is multi-tenant from day one it may have no
legacy docs — then just drop the escape hatch.

**(f) SECURITY — thread MUTATION endpoints have no ownership gate.** Only the
reads were fixed (082e617). In `internal/handler/threads.go`, `ThreadsUpdate`
(`:153`), `ThreadsDelete` (`:196`), `ThreadsMessagesCreate` (`:419`),
`ThreadsMessagesBulkCreate` (`:479`) and `ThreadsMessagesDelete` (`:544`) all
operate on `mux.Vars(r)["id"]` without ever loading the thread doc — any
authenticated user can retitle, delete, or inject messages into any thread by
id. Port the gate to ALL of them: a shared
`requireThreadOwner(ctx, osClient, threadID, userID) error` helper that GETs
the thread doc and returns not-found on mismatch; every mutation handler calls
it first and answers **404** on failure (consistent with `ThreadsGet` — never
reveal existence). For message-delete/bulk endpoints also keep the delete-by-
query scoped by `thread_id` (defence in depth).

**(g) Silent fail-open error swallowing — log degradations.** Instances to fix
while porting: `memory.Add`'s dedup lookup discards `findDuplicate` errors
(`memory.go:87` — an OpenSearch outage silently disables dedup; at least
warn-log); `Search`'s kNN branch falls back to lexical on ANY error with no
log (`memory.go:259-263` — the PR #13 bug survived so long precisely because
this path was silent; warn-log the kNN error before falling back);
`CompositeLog.Read`'s cold-store failure degrades to empty history with no log
(`composite.go:46-51`). Rule for the fork: fail-open is fine as policy, but
every degradation emits one WARN with the underlying error.

## 8. Fork-adaptation notes

**Index mappings first.** `a4651ba` is the cautionary tale: everything
compiled, the hook fired, the log even said `session archived ... messages=1`,
and OpenSearch 400-rejected every assistant doc because `parts` was mapped
`text`. In the fork:

- `messages` mapping MUST have `"parts": { "type": "object", "enabled": false }`
  (`indices.go:94`). If the fork's `messages` index already exists with `parts`
  as `text`, a mapping PUT will conflict — reindex or delete/recreate in dev.
- `messages` also needs `user_id: keyword` (ownership filter), `thread_id`/
  `role`/`model`/`message_group_id: keyword`, `content: text`,
  `created_at: date`. Note `agent_id` and `duration_ms` are written by the
  archiver but are NOT in the static mapping here — they land via dynamic
  mapping. Add them explicitly in the fork (`agent_id: keyword`,
  `duration_ms: long`) rather than trusting dynamic mapping.
- `session_events`: new index, mapping in §4 Phase 2; `payload` must be
  `enabled:false`.
- `memories` (`indices.go:118-143`): `knn: true` index setting,
  `knn.algo_param.ef_search: 256`, vector field
  `{type: knn_vector, dimension: 1024, method: {name: hnsw, space_type:
  cosinesimil, engine: lucene}}`. **Dimension must equal the fork's embedding
  model output** (1024 = intfloat/multilingual-e5-large here). If the fork
  embeds with a different model, change `DefaultVectorDimension` and the
  mapping together. If the fork's engine is not `lucene`, re-read §6 before
  porting the query shapes, and re-derive the dedup threshold if the score
  function differs (0.97 assumes cosinesimil's `(1+cos)/2`).
- `threads` mapping: `user_id: keyword` is what makes the ownership gates
  filterable.

**Other divergence points**:

- Event type constants (`EvTextDelta`, `EvRunStatus`, ...) — map to the fork's
  names; the projector's OUTPUT contract is with the fork's live encoder (§4
  Phase 3).
- The fork may already have its own saver / archive path — the invariant to
  enforce is *exactly one writer of assistant message rows* (the archiver via
  `ProjectMessages`) and *deterministic ids everywhere*.
- `models.yaml` (88e738d): don't replay the diff; instead grep the fork's
  model list for duplicate ids and dedupe what IT has.
- Redis client: this repo uses `valkey-go` builders. Any client works — the
  Lua script and key scheme are the contract.
- If the fork's HTTP framework differs (chi/echo/etc.), `threads.go` route
  shapes will differ; port handler behaviour + gates, not mux specifics.

## 9. Verification

**Unit tests** (must be green before any live checks):

```bash
go test ./internal/eventlog/ -run TestProjectMessages -v   # all 13
go test ./internal/eventlog/ -run 'Composite|RedisStream|Heartbeat'
go test ./internal/agent/ -run 'Hook|Coordinator|Archive'
go build ./...
```

**Durability round-trip** (live Valkey + OpenSearch):

```bash
# 1. run a turn with a tool call
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"agent_id":"assistant","stream":false,"thread_id":"t-verify-1",
       "messages":[{"role":"user","content":"what time is it? use a tool"}]}'

# 2. archive landed (raw events + projected message, deterministic ids)
curl -s 'localhost:9200/session_events/_search?q=session_id:t-verify-1&size=0' | jq .hits.total.value
curl -s 'localhost:9200/messages/_doc/t-verify-1:0:assistant' | jq '._source | {role, model, agent_id, parts: (.parts|length)}'
curl -s 'localhost:9200/messages/_doc/t-verify-1:0:user' | jq '._source.user_id'   # must be non-empty (7687842)

# 3. reload equals live: parts include the dynamic-tool part
curl -s -H "X-User-Id: $OWNER" localhost:8080/v1/threads/t-verify-1/messages | jq '.[1].parts[].type'

# 4. cold cutover: expire the hot keys, reload must still return full history
redis-cli DEL "evlog:agentic:t-verify-1" "evlog:seq:agentic:t-verify-1"
curl -s -H "X-User-Id: $OWNER" localhost:8080/v1/threads/t-verify-1/messages | jq length

# 5. ownership (after §7f): every one of these must 404 for a non-owner
curl -s -o /dev/null -w '%{http_code}\n' -H "X-User-Id: intruder" localhost:8080/v1/threads/t-verify-1
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH  -H "X-User-Id: intruder" localhost:8080/v1/threads/t-verify-1 -d '{"title":"pwn"}'
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "X-User-Id: intruder" localhost:8080/v1/threads/t-verify-1
curl -s -o /dev/null -w '%{http_code}\n' -X POST   -H "X-User-Id: intruder" localhost:8080/v1/threads/t-verify-1/messages -d '{"role":"user","content":"x"}'
```

**Memory API checks** (live stack; the exact scenario PR #13 was verified on):

```bash
# seed two distinct facts for one user
curl -s -X POST localhost:8080/v1/memories -H "X-User-Id: u1" -d '{"content":"Favorite programming language: Rust"}'
curl -s -X POST localhost:8080/v1/memories -H "X-User-Id: u1" -d '{"content":"Works at Prism Group"}'

# dedup: near-duplicate returns the EXISTING id, no new doc
curl -s -X POST localhost:8080/v1/memories -H "X-User-Id: u1" -d '{"content":"favorite language is Rust"}'
curl -s 'localhost:9200/memories/_count?q=user_id:u1' | jq .count        # still 2

# junk rejection: never persisted
curl -s -X POST localhost:8080/v1/memories -H "X-User-Id: u1" -d '{"content":"Work: NONE"}'   # rejected

# COMBINED kNN recall — the query that returned 0/partial hits pre-fix must now return BOTH facts
curl -s 'localhost:8080/v1/memories/search?q=what%20language%20does%20the%20user%20like%20and%20where%20do%20they%20work' \
  -H "X-User-Id: u1" | jq '.[].content'          # expect Rust AND Prism Group

# cross-user isolation
curl -s 'localhost:8080/v1/memories/search?q=Rust' -H "X-User-Id: u2" | jq length   # 0
```

And the raw combined kNN+filter query directly against OpenSearch (proves the
§6 shape works on the fork's engine — substitute a real embedding vector):

```bash
curl -s localhost:9200/memories/_search -H 'content-type: application/json' -d '{
  "size": 5,
  "query": { "knn": { "vector": {
      "vector": '"$QUERY_EMBEDDING"', "k": 5,
      "filter": { "bool": { "filter": [
        { "term": { "app_name": "agentic" } },
        { "term": { "user_id": "u1" } } ] } } } } }
}' | jq '.hits.hits[] | {score: ._score, content: ._source.content}'
# non-empty hits with sane scores; then re-run with the filter as a bool.must
# SIBLING and observe 0 hits — confirming the fork is on the fixed shape.
```

**Regression sweep**: >10k-event handling behaves per whichever §7b option the
fork chose; archive flush of a large session is a handful of `_bulk` calls
(watch OpenSearch access logs), and a deliberately broken OpenSearch URL makes
`Flush` return a non-nil aggregated error.
