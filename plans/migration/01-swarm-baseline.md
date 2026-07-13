# 01 — Swarm Baseline: Typed Roster, Event-Sourced Sessions, Task/Question Tools, MCP Client, Office MCP Server

> **Audience**: a coding agent porting this feature set onto a fork of this repo that
> diverged *before* any of this work landed. The fork has its own local changes that
> must be preserved. **Implement/port — do not blind cherry-pick.** Use this repo at
> main (`17b1a87`) as the reference implementation; all file:line citations below are
> against that commit.
>
> **Origin commit**: `ab9fb39` ("swarm baseline: typed roster, event-sourced sessions,
> task/question tools, MCP client, office MCP server"). Diff it for the original shape
> (`git diff ab9fb39^..ab9fb39 --stat`), but **port from current main**, because later
> phases (W1–W6 remediation, Tasks A–C, M1–M5, H1–H5 hardening) refined nearly every
> file introduced there. This doc describes the *current-main* form and flags the
> refinements inline so you don't re-introduce fixed bugs.

---

## 1. Purpose & Architecture Overview

This baseline turns a request-scoped, single-agent chat gateway into a
**background-capable, multi-agent swarm orchestrator**:

- **Typed agent roster** (`internal/roster`): a registry of agent definitions sourced
  from YAML *and* opencode-style markdown files (frontmatter + prompt body). It powers
  dynamic subagent dispatch and per-agent tool permissions.
- **Event-sourced sessions** (`internal/eventlog` + `internal/agent`): every run writes
  typed `AgentEvent`s to a durable, per-session, sequence-numbered log (in-memory or
  Redis Streams). The HTTP connection becomes a *reader* of that log, so runs survive
  disconnects and clients resume exactly-once with `?after=<seq>`.
- **`task` / `task_join` tools** (`internal/tools/task.go`): a coordinator agent
  dispatches subtasks to roster subagents as child sessions, foreground or background
  (parallel), with the child's events streamed into the *parent* session log so the UI
  renders live sub-agent cards.
- **`question` tool** (`internal/tools/question.go`): HITL-style interactive questions;
  the run suspends (`awaiting-input`) and resumes with the user's structured answers.
- **MCP client** (`internal/mcp` + `config/default/mcp.yaml`): the *backend* connects to
  MCP servers (remote streamable-HTTP or local stdio) and exposes their tools to agents
  that opt in via `mcp_servers:` — required because swarm runs continue after the
  browser disconnects, so credentials/tools must live server-side.
- **Office MCP server** (`services/office-mcp`): a Python FastMCP server generating
  docx/pptx/xlsx files, served as the first real remote MCP integration.

### Component diagram

```
                 POST /v1/chat/completions          GET /v1/sessions/{id}/stream?after=N
                          │                                     │
                          ▼                                     ▼
                 ┌─────────────────┐                 ┌────────────────────┐
                 │ handler.Chat    │                 │ handler.Session*   │
                 │ (agent_id, auto)│                 │ (list/status/attach│
                 └───────┬─────────┘                 │  /cancel/viewed)   │
                         │ Start(RunRequest)         └─────────┬──────────┘
                         ▼                                     │ Read(after, follow)
              ┌──────────────────────┐                         │
              │ agent.Coordinator    │  run goroutine          │
              │ (background runs,    ├────────┐                │
              │  queueing, terminals,│        ▼                │
              │  Resume, Cancel)     │  eventLogEncoder        │
              └──────────┬───────────┘  (stream.Encoder →      │
                         │              AgentEvent Append)     │
                         │                      │              │
                         │                      ▼              ▼
                         │            ┌───────────────────────────────┐
                         │            │ eventlog.EventLog (port)      │
                         │            │  ├─ MemoryLog   (default)     │
                         │            │  └─ RedisStreamLog (Valkey)   │
                         │            └───────────────┬───────────────┘
                         │                            │ SeqEvent channel
                         │                            ▼
                         │                   agent.PumpEventLog
                         │                   (replay→live, SSE encoder,
                         │                    RunAttach | SessionFollow)
                         │
       tools on the running agent (built by shared.BuildLLMAgent)
                         │
     ┌───────────────────┼──────────────────────┬──────────────────┐
     ▼                   ▼                      ▼                  ▼
┌──────────┐      ┌─────────────┐        ┌────────────┐    ┌──────────────┐
│ task /   │      │ question    │        │ MCP toolsets│   │ regular tools│
│ task_join│      │ (HITL pause)│        │ (mcp.Manager│   │ (rag, web,…) │
└────┬─────┘      └─────────────┘        │  per-agent) │   └──────────────┘
     │  BuildChild(def) + child Runner   └──────┬──────┘
     ▼                                          │ streamable HTTP / stdio
┌──────────────────┐                            ▼
│ roster.Registry  │                   ┌──────────────────┐
│ (Definitions from│                   │ office-mcp (py)  │
│  agents.yaml +   │                   │ create_pptx,     │
│  agents/*.md)    │                   │ render_report_docx│
└──────────────────┘                   │ create_xlsx      │
                                       └──────────────────┘
```

Child dispatch data flow: `task` tool → `roster.Registry.Get(subagent_type)` →
`BuildChild` (bootstrap closure over `shared.BuildLLMAgent`) → new ADK `runner.Runner`
with child session id `parent:type-shortid` → child SSE events translated
(`translateChildEvent`) → appended to the **parent** session's event log with
`Author`/`SubagentType`/`SessionID` attribution.

---

## 2. Key Concepts & Invariants

Internalize these before writing code; most later-phase bugfixes were violations of
one of them.

1. **The event log is the source of truth for a session's stream.** Live clients and
   reconnecting clients render *the same bytes* because both go through
   `PumpEventLog` replaying `AgentEvent`s through a real wire encoder. Nothing is
   written to the HTTP response that isn't in the log (heartbeats excepted, `Seq==-1`).

2. **Per-session sequence numbers are monotonic and gap-free.** `Append` assigns
   `seq` (memory: slice index+1; Redis: Lua-atomic `INCR`+`XADD` with entry id
   `<seq>-0`). `Read(after, follow)` = replay backlog `> after`, then live tail, with
   gap-fill from the durable backlog if a live fan-out copy was dropped
   (`internal/eventlog/memory.go:145-171`). Resume is `?after=<seq>`.

3. **Terminal state belongs to RUNS, not sessions.** One session log carries many
   runs/turns. A terminal `run-status` event must **never** close a follow reader in
   the log layer — closure policy lives in the pump (`PumpRunAttach` closes at first
   terminal; `PumpSessionFollow` closes only on client disconnect). This was the W1
   remediation; the log backends both carry comments enforcing it.

4. **Exactly one terminal per run.** `RunHandle.termOnce` (a `sync.Once`) guards the
   terminal append + status flip; Cancel and natural finish race safely
   (`internal/agent/coordinator.go:492-562`).

5. **Roster typing is derived, not duplicated.** `roster.Definition` is built *from*
   `config.AgentConfig` (the YAML DTO). Markdown agent files merge into the same
   `AgentsConfig` before `roster.FromAgentsConfig` runs. `CanDispatch` is derived from
   the tool list containing `"task"` — no separate type map.

6. **HITL/question interception suspends, never blocks.** The `question` tool uses
   ADK's `RequireConfirmation: true`. The interrupt reaches the encoder as
   `ToolInterrupt` → `EvQuestion` event + `interrupted=true`; the coordinator maps the
   run to `awaiting-input` (not done, not error). Resume re-enters via
   `Coordinator.Resume`, packing answers into the ADK confirmation
   `FunctionResponse` payload; the tool handler then runs *for the first time* and
   returns the answers to the model. The encode keys (`payload`/`answers`/`text`) in
   `internal/agent/coordinator.go:760-765` must stay in lockstep with the decode keys
   in `internal/tools/question.go:19-23`.

7. **Spawned children are denied `question` and (by default) `task`.** A dispatched
   child has no HITL resume path — allowing `question` would deadlock. The bootstrap
   `buildChild` closure strips those tools; `translateChildEvent` additionally flags
   `st.blocked` if a child still emits a confirmation request, and the task tool
   returns a failed result instead of hanging (`internal/tools/task.go:303-308`).

8. **Attribution is resolved once, at write time.** Child events carry
   `Author` ("type#shortid" label), `SubagentType`, `SessionID` (child id), `Step`.
   Readers never need the run's in-memory state to attribute events.

9. **Everything degrades gracefully without external services.** No Valkey → memory
   log. No OpenSearch → skills manifest and archive are skipped. Bad MCP config → the
   server is `failed` and skipped, never a startup panic (H3c). Missing `mcp.yaml` or
   `config/<env>/agents/` dir → empty, not error.

10. **User ownership is enforced at the coordinator** (H2): a session last-known to
    another user cannot be started, resumed, streamed, or cancelled by this user;
    handlers 404 (never 403, to avoid existence leaks).

---

## 3. File Inventory

Files to create (or merge into existing fork counterparts). Paths cite current main.
"New" = did not exist before `ab9fb39` in this repo; your fork may still differ.

### Roster

| File | Role |
|---|---|
| `internal/roster/permissions.go` | `Permissions` glob ruleset (last-match-wins), `ReadOnlyPermissions()` canonical deny set. Stdlib-only leaf package. |
| `internal/roster/definition.go` | `Definition` (typed agent), `Mode` (primary/subagent/all), `fromAgentConfig` derivation incl. `CanDispatch`. |
| `internal/roster/registry.go` | `Registry`: `Get/Primary/Dispatchable`, `Manifest(allowed)` renders `<available_subagents>` for the task tool description. |
| `internal/roster/load_markdown.go` | `LoadMarkdownDir`: parse `*.md` (YAML frontmatter + body-as-prompt), upsert into `AgentsConfig` by id. |
| `internal/roster/permissions_test.go` | Ruleset tests (legacy deny-set parity, last-match-wins, order-preserving filter). |

### Event log

| File | Role |
|---|---|
| `internal/eventlog/event.go` | `AgentEvent` union: `EventType` consts, payload structs (`ToolPayload`, `TaskItem`, `UsagePayload`, `QuestionPayload`), `IsTerminal()`. |
| `internal/eventlog/eventlog.go` | The `EventLog` port: `Append/Read/Head/Close`, `SeqEvent`. |
| `internal/eventlog/memory.go` | `MemoryLog`: backlog + best-effort fan-out + gap-fill + idle heartbeat + `Evict`/`IdleSince` for the sweeper. |
| `internal/eventlog/redis_stream.go` | `RedisStreamLog`: Lua-atomic INCR+XADD, XRANGE replay, XREAD BLOCK tail, heartbeats on block timeout. |
| `internal/eventlog/memory_test.go` | Contract tests (monotonic seq, replay, resume-after, replay-then-live, terminal-doesn't-close-follow). |

Later-phase files in the same package (`composite.go`, `project.go`, `taskstate.go`,
`heartbeat_test.go`, …) belong to later migration docs — but note `bootstrap` at main
references `NewCompositeLog` and `TaskBoardStore`; see §6 for how to stub if you port
this doc standalone.

### Run coordinator / pump (agent package)

| File | Role |
|---|---|
| `internal/agent/coordinator.go` | `Coordinator`: Start (with per-session queueing), run goroutine, `terminate` (once-guarded), `Resume` (HITL/question), `Cancel`, `Status/List`, idle sweeper, post-run/start hooks. |
| `internal/agent/eventlog_encoder.go` | `eventLogEncoder`: implements the repo's `stream.Encoder` interface by appending typed events; sets `interrupted` on `ToolInterrupt`. |
| `internal/agent/pump.go` | `PumpEventLog`: replays `AgentEvent`s through a real encoder; `PumpRunAttach` vs `PumpSessionFollow` closure policy. |
| `internal/agent/eventlog_factory.go` | `NewEventLog(cfg)`: `EVENTLOG_STORE=redis|valkey` → RedisStreamLog, else MemoryLog; graceful fallbacks. |
| `internal/agent/stream_coordinator.go` | `StreamAgentRunBackground` (chat POST → Start + run-attach pump) and `StreamSessionAttach` (reconnect endpoint). |
| `internal/agent/coordinator_test.go` | Multi-turn, queueing, error-status, cancel-no-double-terminal, sweep tests. |

### Tools

| File | Role |
|---|---|
| `internal/tools/task.go` | `TaskArgs/TaskResult/TaskDeps/TaskHub`, `NewTaskTool` (dispatch, fg/bg + semaphore), `runChild`, `NewTaskJoinTool`. |
| `internal/tools/child_stream.go` | `translateChildEvent` (pure ADK-event→AgentEvent translation), `childStreamState`, `childLogSink`. |
| `internal/tools/question.go` | `NewQuestionTool` (RequireConfirmation), payload decode, opencode-format answer summary. |
| `internal/tools/registry.go` | Adds `task`/`task_join`/`question` names, `Deps.TaskTool/TaskJoinTool/TaskFactory/MCPToolsets`, `HITLToolNames()` includes `question`. |
| `internal/tools/task_test.go`, `question_test.go` | Translation, hub fan-in, monotonic task list, payload round-trip tests. |

### MCP client

| File | Role |
|---|---|
| `internal/config/mcp.go` | `MCPConfig`/`MCPServerConfig` DTOs, `${ENV}` expansion (`ExpandEnv`, `ExpandedHeaders`), `LoadMCP` (missing file = empty). |
| `internal/mcp/manager.go` | `Manager`: per-server ADK toolset (lazy connect), status reporting, transport selection (stdio vs streamable HTTP). |
| `config/default/mcp.yaml` | Server declarations (office enabled by default at `http://localhost:8090/mcp`). |

`internal/mcp/oauth*.go`, `roundtripper.go`, `resilient.go`, `handlers.go`,
`tokenstore.go` (backend-held OAuth + resilient toolset + `/v1/mcp` routes) arrived
with the fuller Phase-04 work; port them together with the manager if you want the
`/v1/mcp` endpoints — the manager at main depends on `NewOAuthProvider`,
`newAuthHTTPClient`, and `resilientToolset`, so either take those files too (they are
self-contained in `internal/mcp/`) or trim the manager (see §6).

### Agent definitions & config

| File | Role |
|---|---|
| `config/default/agents/swarm-coordinator.md` | The dynamic swarm coordinator (primary agent, tools: task/task_join/todowrite/emit_artifact/question, `mcp_servers: [office]`, `allowed_subagents`). |
| `config/default/agents/researcher.md`, `data-analyst.md`, `gap-analyst.md`, `report-writer.md` | `mode: subagent` specialist leaves for dispatch. |
| `internal/config/agents.go` | `AgentConfig` gains `Mode`, `AllowedSubagents`, `ReadOnly`, `AppendVerdict`, `InjectSkillsManifest`, `MCPServers`, `Clone()`. |
| `agents/shared/shared.go` | `BuildLLMAgent` gains: read-only tool filtering via roster permissions, governed TaskFactory swap-in, skills manifest / verdict / tool-discipline prompt suffixes, MCP toolset attachment. |

### Wiring & HTTP

| File | Role |
|---|---|
| `internal/bootstrap/bootstrap.go` | Wires everything: markdown merge → registry, event log, coordinator, `buildChild`, `TaskFactory`, MCP manager, `Result` fields. |
| `internal/server/server.go` | Routes: `/v1/sessions*`, `/v1/mcp*`, `/v1/route`, `/v1/agent/resume`. |
| `internal/handler/sessions.go` | List/status/stream-attach/cancel(/viewed) handlers with H1 `after` validation and H2 ownership 404s. |
| `internal/handler/route.go` | `ClassifyAgent` + `/v1/route` (auto agent routing seam). |
| `internal/handler/identity.go` | `UserID(r)` single identity seam (X-User-ID header, `anonymous` fallback). |
| `internal/hitl/hitl.go` (pre-existing) | `PendingInterrupt` — reused by `Coordinator.Resume`. |

### Office MCP server

| File | Role |
|---|---|
| `services/office-mcp/server.py` | FastMCP streamable-HTTP server: `render_report_docx`, `create_pptx`, `create_xlsx`; serves generated files at `/files/{name}` on the same port. |
| `services/office-mcp/make_template.py` | Builds the default docxtpl `report.docx` template programmatically. |
| `services/office-mcp/requirements.txt` | `mcp`, `docxtpl`, `python-pptx`, `openpyxl`, etc. |

### Go module additions

`go.mod` (main): `github.com/modelcontextprotocol/go-sdk v0.7.0`,
`github.com/valkey-io/valkey-go v1.0.72`; ADK is `google.golang.org/adk v0.5.0`
(the task tool uses `runner`, `session`, `tool/functiontool`,
`tool/toolconfirmation`, `tool/mcptoolset`).

---

## 4. Step-by-Step Implementation Order

Each step compiles independently; do them in order — later steps consume earlier ones.

**Step 1 — roster package (no deps beyond `internal/config`).**
Create `internal/roster/{permissions,definition,registry,load_markdown}.go` +
`permissions_test.go`. Extend your fork's `AgentConfig` with the new fields (see §3
table) and `Clone()`. Call nothing yet.
✅ Checkpoint: `go build ./...` and `go test ./internal/roster/` pass
(3 permission tests).

**Step 2 — eventlog package (leaf, stdlib + valkey-go).**
Create `internal/eventlog/{event,eventlog,memory,redis_stream}.go` + `memory_test.go`.
If your fork lacks a Valkey client wrapper (`pkg/db/valkey` here), either port it or
make the Redis backend construction take a `valkey.Client` directly (it already does —
only the *factory* needs your fork's config plumbing).
✅ Checkpoint: `go test ./internal/eventlog/` passes, especially
`TestMemoryLogTerminalBeforeFollowStaysLive` (the W1 invariant) and
`TestMemoryLogResumeAfterSeq`.

**Step 3 — event-log encoder + pump + factory.**
Create `internal/agent/{eventlog_encoder,pump,eventlog_factory}.go`. These adapt the
log to your fork's streaming layer: `eventLogEncoder` must implement **your fork's**
`stream.Encoder` interface (here: `internal/stream`), and `PumpEventLog` must call the
same interface. If your fork's encoder interface differs (fewer methods, different
signatures), adapt method-by-method — the mapping table is §5.3. Add
`EVENTLOG_STORE` to your config struct.
✅ Checkpoint: compiles; `var _ stream.Encoder = (*eventLogEncoder)(nil)` type-checks.

**Step 4 — run coordinator.**
Create `internal/agent/coordinator.go` (+ `stream_coordinator.go`) and port
`coordinator_test.go`. The production `defaultRunFunc` calls your fork's existing
run-a-turn function (here `streamEvents(ctx, enc, core, sessionID, runID, content,
saver, logger)` — locate your fork's equivalent that drives an ADK runner into an
encoder) — keep the `runFunc` seam so tests inject a stub. If your fork has no
`chat.MessageSaver`/persistence, pass nil and drop those branches.
✅ Checkpoint: `go test ./internal/agent/ -run TestCoordinator` passes
(multi-turn, queued, error, cancel, sweep).

**Step 5 — question tool.**
Create `internal/tools/question.go` + `question_test.go`; register `"question"` in
your tool registry and in `HITLToolNames()`. Verify your fork's HITL interception
path (however it surfaces `RequireConfirmation` interrupts) reaches the encoder as a
`ToolInterrupt` so `EvQuestion` gets logged and `interrupted` gets set. Wire
`Coordinator.Resume`'s `buildConfirmationResponse` to your resume HTTP handler
(here `POST /v1/agent/resume`, using the pre-existing `hitl.PendingInterrupt` store).
✅ Checkpoint: `go test ./internal/tools/ -run TestQuestion` +
`TestParseConfirmationPayload_ToleratesJSONShapes` pass.

**Step 6 — task/task_join tools.**
Create `internal/tools/{task,child_stream}.go` + `task_test.go`. This is the largest
adaptation surface: it needs your fork's ADK version's `runner.New`,
`session.Service.Create`, and `functiontool.New` signatures. Keep `translateChildEvent`
pure so the tests port verbatim.
✅ Checkpoint: `go test ./internal/tools/` fully green (translation, hub fan-in,
monotonic task list).

**Step 7 — MCP client.**
Create `internal/config/mcp.go`, `internal/mcp/manager.go` (plus
`resilient.go`/`roundtripper.go`/`oauth*.go`/`handlers.go` if you want auth + the
`/v1/mcp` routes), and `config/default/mcp.yaml`. Add
`github.com/modelcontextprotocol/go-sdk` to go.mod. Wire `Deps.MCPToolsets` and the
`llmCfg.Toolsets` attachment in your `BuildLLMAgent` equivalent.
✅ Checkpoint: `go test ./internal/mcp/ -run TestNewManager_NoPanicOnBadConfig` passes
(empty command, empty URL, unknown type all degrade to `failed`, no panic).

**Step 8 — builder + bootstrap wiring.**
Update `BuildLLMAgent` (read-only filter, TaskFactory swap-in, MCP toolsets,
prompt suffixes) and bootstrap: markdown merge → `roster.FromAgentsConfig` → event
log → coordinator → `buildChild` closure → `deps.TaskFactory` → MCP manager →
`Result` fields. Order matters: the registry must exist before the TaskFactory, and
`deps.MCPToolsets` before any agent is built.
✅ Checkpoint: `go build ./... && go vet ./...`; server boots with logs
"agents config loaded" (with primary/dispatchable counts), "swarm: task dispatch +
join tools ready", "mcp: server registered".

**Step 9 — HTTP surface.**
Add routes + handlers: `GET /v1/sessions`, `GET /v1/sessions/{id}`,
`GET /v1/sessions/{id}/stream` (H1: reject negative/invalid `after` with 400),
`POST /v1/sessions/{id}/cancel`, `/v1/route`, and switch your chat POST handler to
`StreamAgentRunBackground` (or add it as an opt-in path first — see §6.5).
✅ Checkpoint: full `make test` green; manual smoke (§7) passes.

**Step 10 — agent definitions + office server.**
Add `config/default/agents/*.md` (coordinator + subagents) adapted to your fork's
models/providers, and `services/office-mcp/`.
✅ Checkpoint: manual smoke §7.3.

---

## 5. Key Code Excerpts (main @ 17b1a87)

### 5.1 Roster

`internal/roster/definition.go:22-41` — the typed definition:

```go
type Definition struct {
    Name        string // stable id (AgentConfig.ID)
    DisplayName string
    Description string
    Mode        Mode        // primary | subagent | all
    Model       string
    Provider    string
    Tools       []string
    Permissions Permissions // resolved tool ruleset (read-only agents, etc.)
    ReadOnly             bool
    AppendVerdict        bool
    InjectSkillsManifest bool
    CanDispatch          bool     // tool list includes "task" → may spawn sub-agents
    AllowedSubagents     []string // governance: empty = all
    Source string                 // "yaml" | "<file>.md" | "code"
    cfg *config.AgentConfig       // back-ref so orchestrator builders keep working
}
```

Note `CanDispatch: slices.Contains(ac.Tools, "task")` (`definition.go:77`) and the
type-driven defaults switch (`definition.go:82-94`): explore/plan → ReadOnly;
verification → ReadOnly+AppendVerdict; basic/codeguide/"" → InjectSkillsManifest.

`internal/roster/permissions.go:30-47` — last-match-wins glob rules:

```go
type Permissions struct {
    Default Effect // EffectAllow if empty
    Rules   []Rule // evaluated top-to-bottom; the LAST matching rule decides
}
func (p Permissions) Allowed(name string) bool {
    eff := p.Default
    if eff == "" { eff = EffectAllow }
    for _, r := range p.Rules {
        if ok, _ := path.Match(r.Glob, name); ok { eff = r.Effect }
    }
    return eff == EffectAllow
}
```

`ReadOnlyPermissions()` (`permissions.go:66-75`) denies `write_*`, `*_memory`,
`trigger_alert` — deliberately leaving `*_memories` (read-only) allowed.

`internal/roster/registry.go:63-87` — `Manifest(allowed)` renders the deterministic,
sorted `<available_subagents>` block injected into the task tool's description.

`internal/roster/load_markdown.go:42-91` — `LoadMarkdownDir(ac, dir)`: missing dir is
not an error; frontmatter id defaults to the filename; `upsertAgent` **replaces** a
YAML entry with the same id (markdown wins).

### 5.2 Event log

`internal/eventlog/event.go:15-31` — the event union discriminators:

```go
EvRunStatus, EvTextDelta, EvReasoningDelta, EvToolCall, EvToolResult,
EvAgentStep, EvAgentDelta, EvTaskList, EvArtifact, EvUsage,
EvQuestion, EvHITLResolved, EvProgress, EvMetadata, EvHeartbeat
```

`AgentEvent` (`event.go:64-94`) is flat+omitempty; attribution
(`InvocationID/Author/SubagentType/SessionID/Step/IsOutput`) is stamped at write time.
`IsTerminal()` (`event.go:141-145`) — run-status of done/error/cancelled/awaiting-input.

`internal/eventlog/eventlog.go:18-38` — the port:

```go
type EventLog interface {
    Append(ctx context.Context, sessionID string, ev AgentEvent) (seq int64, err error)
    Read(ctx context.Context, sessionID string, afterSeq int64, follow bool) (<-chan SeqEvent, error)
    Head(ctx context.Context, sessionID string) (int64, error)
    Close() error
}
```

Contract notes in the docstring are load-bearing: replay-then-live, terminal does NOT
close follow readers, negative `afterSeq` clamps to 0, single writer per session.

`internal/eventlog/memory.go:103-118` — snapshot backlog AND register subscriber under
one lock hold (no append can slip between); the deferred unlock is the H1
mutex-poisoning fix. Gap-fill on dropped live copies: `memory.go:160-171` via
`replayRange`.

`internal/eventlog/redis_stream.go:49-61` — the atomic append (H4 fix):

```lua
local seq = redis.call('INCR', KEYS[2])
-- XADD with entry id seq.."-0" (MAXLEN ~ trim optional), EXPIRE both keys
return seq
```

Keys: stream `evlog:{app}:{session}`, counter `evlog:seq:{app}:{session}`; TTL 24h,
`blockMS` 15000 (XREAD timeout doubles as the heartbeat cadence), MAXLEN ~10000.

### 5.3 Encoder ↔ pump mapping

`internal/agent/eventlog_encoder.go` (write side) and `internal/agent/pump.go:36-127`
(read side) are exact mirrors. Key rows:

| stream.Encoder call | AgentEvent | Pump replay |
|---|---|---|
| `Text(delta)` | `EvTextDelta{IsOutput:true}` | `enc.Text` |
| `ToolCall(idx,id,name,args)` | `EvToolCall{IsOutput:true,Step:idx}` | `enc.ToolCall` (IsOutput) else `enc.AgentToolCall` |
| `AgentText(agent,step,delta)` | `EvAgentDelta{Kind:text}` | `enc.AgentText` |
| `TaskList(tasks)` | `EvTaskList` | `enc.TaskList` |
| `ToolInterrupt(i)` | `EvQuestion` + `interrupted=true` | `enc.ToolInterrupt`; **RunAttach mode returns here** (pause framing) |
| (coordinator terminate) | `EvRunStatus{done/error/…}` | `RunFailed(err)` on error else `RunFinished(lastUsage)`; RunAttach returns, SessionFollow resets usage and keeps going |

M3 subtleties baked in at main: `RunFinished` on the encoder is a **no-op** (usage
already logged as `EvUsage`; re-emitting double-counted on replay), and
`ToolInterrupt` extracts the structured `questions` list from the tool args
(`eventlog_encoder.go:130-157`) so the UI renders a real question card.
`EvHITLResolved` replays as a re-surfaced ToolCall (`pump.go:100-104`).

### 5.4 Coordinator

`internal/agent/coordinator.go:308-338` — Start with per-session queueing (C2):

```go
if h, ok := c.active[req.SessionID]; ok && (h.Status == RunRunning || h.Status == RunQueued) {
    c.pending[req.SessionID] = append(c.pending[req.SessionID], req) // never dropped
    ... return queued handle (StartSeq approximate)
}
h := c.startLocked(req)
```

`startLocked` (`coordinator.go:342-386`): stamps `StartSeq = Head()+1`, appends
`run-status{running}`, launches `go c.run(...)`. The run goroutine
(`coordinator.go:415-426`):

```go
enc := newEventLogEncoder(ctx, c.log, req.SessionID)
outcome := c.runFn(ctx, req, enc)
if enc.Interrupted() { outcome = runOutcome{status: RunAwaitingInput} } // HITL wins
c.finish(ctx, req, h, runID, outcome)
```

`terminate` (`coordinator.go:492-562`) — the once-guarded terminal; `finish` then
drains the next queued turn. `Resume` (`coordinator.go:589-655`) — re-enters with a
`genai.FunctionResponse{Name: toolconfirmation.FunctionCallName, ID:
pending.ConfirmationCallID, Response: buildConfirmationResponse(approved, answers,
text)}` and first logs `EvHITLResolved` with `Kind: approved|denied`.

Run-attach read (`internal/agent/stream_coordinator.go:53-61`): attach at
`h.StartSeq-1` so the reader sees only THIS run's events — this is what makes
multi-turn streaming work.

### 5.5 Task tool

`internal/tools/task.go:30-43` — the schema:

```go
type TaskArgs struct {
    SubagentType string `json:"subagent_type" ...`
    Description  string `json:"description" ...`   // 3-7 word UI card title
    Prompt       string `json:"prompt" ...`        // self-contained; child sees no parent convo
    Background   bool   `json:"background,omitempty" ...`
}
type TaskResult struct { SessionID, Status, Result string } // completed|running|failed
```

Dispatch core (`task.go:184-231`): registry lookup + allowed check → `BuildChild(def)`
→ own `runner.New{AppName, Agent, SessionService}` → child id
`parentID + ":" + type + "-" + short` (M5: `:` is mux-safe so
`/v1/sessions/{childID}` works) → background path takes the hub semaphore in a
`context.WithoutCancel(tc)` goroutine (M2: outlives the tool call, still bounded);
foreground shares the same semaphore and inherits `tc` cancellation.

`runChild` (`task.go:236-310`): mints the child session with
`_meta:parentID/subagentType/description` state; emits `EvAgentStep{started}` →
translated child events → `EvAgentStep{done, duration}`; maintains the
spawn-synthesised `EvTaskList` snapshots via `TaskHub.upsertTask` (settled statuses
never regress, `task.go:95-126`); returns `failed` + partial text if the child
`blocked` on a confirmation.

`translateChildEvent` (`internal/tools/child_stream.go:32-104`): partials → agent
deltas; non-partials join text parts into `st.finalText` (last full message wins;
fixes the M2 truncation bug) and skip re-emitting text that already streamed
(`hadPartial`); `adk_request_confirmation` FunctionCall → `st.blocked = true`.

### 5.6 Question tool

`internal/tools/question.go:34-47` — opencode-compatible schema (`QuestionItem`:
question, header chip, options, multiple, custom free-text). `NewQuestionTool`
(`question.go:60-68`) sets `RequireConfirmation: true`; `questionHandler` runs only
after resume and decodes `ctx.ToolConfirmation().Payload` (JSON round-trip tolerant:
`[][]string` | `[]any` shapes, `question.go:106-155`), producing both structured
`Answers` and a model-facing `Summary` string.

### 5.7 MCP

`internal/mcp/manager.go:189-212` — transport selection:

```go
case "local":  // stdio; empty command → error → StatusFailed, skipped (H3c)
    cmd := exec.Command(sc.Command[0], sc.Command[1:]...)
    return &mcpsdk.CommandTransport{Command: cmd}, nil
case "remote", "": // streamable HTTP; auth via custom RoundTripper
    client := newAuthHTTPClient(name, sc, m.oauth)
    return &mcpsdk.StreamableClientTransport{Endpoint: sc.URL, HTTPClient: client}, nil
```

Toolsets are built eagerly but connect lazily (first `Tools()` call), wrapped in
`resilientToolset` so an unreachable server degrades instead of failing the agent
build. Agents attach via `agents/shared/shared.go:120-122`:

```go
if len(agentCfg.MCPServers) > 0 && deps.MCPToolsets != nil {
    llmCfg.Toolsets = deps.MCPToolsets(agentCfg.MCPServers)
}
```

`internal/config/mcp.go:41-66` — `${ENV}` expansion for header values and OAuth
client credentials (H3b).

### 5.8 Bootstrap wiring (the recipe)

`internal/bootstrap/bootstrap.go:177-190` — roster construction:

```go
agentsCfg, err := config.LoadAgents(filepath.Join(configDir, "agents.yaml")) // required
roster.LoadMarkdownDir(agentsCfg, filepath.Join(configDir, "agents"))        // md overrides yaml by id
cfg.Agents = agentsCfg
reg := roster.FromAgentsConfig(agentsCfg)
```

`bootstrap.go:307-348` — the child builder + governed task factory:

```go
buildChild := func(def *roster.Definition) (adkagent.Agent, error) {
    cc := def.Config().Clone()
    if !def.CanDispatch { cc.Tools = filterOut(cc.Tools, "task", "task_join") }
    cc.Tools = filterOut(cc.Tools, "question") // children can NEVER ask (no resume path)
    return shared.BuildLLMAgent(cfg, cc, deps)
}
deps.TaskFactory = func(allowed []string) (tool.Tool, tool.Tool, error) {
    hub := tools.NewTaskHub(0) // one hub/semaphore per coordinator instance
    td := tools.TaskDeps{Registry: reg, AppName: cfg.AppName, SessionService: sessionService,
        EventLog: eventLog, BuildChild: buildChild, Allowed: allowed, Hub: hub}
    ...
}
```

And in `shared.BuildLLMAgent` (`agents/shared/shared.go:71-79`), an agent whose tool
list contains `task`/`task_join` gets a factory-built pair restricted to its
`AllowedSubagents` — per-coordinator governance instead of one global tool.

---

## 6. Fork-Adaptation Notes

Your fork diverged pre-baseline and has local changes. Expected conflict zones and
how to adapt **rather than overwrite**:

### 6.1 `AgentConfig` / agents.yaml format
- Add the new fields (`Mode`, `AllowedSubagents`, `ReadOnly`, `AppendVerdict`,
  `InjectSkillsManifest`, `MCPServers`, `Clone()`) to your fork's existing struct —
  do not replace the struct; every field is `omitempty`/additive so existing YAML
  keeps parsing. If your fork renamed fields (e.g. `prompt` vs `system_prompt`), map
  the markdown frontmatter keys in `load_markdown.go` to *your* names — the
  frontmatter struct is the only place the key set is spelled out.
- If your fork already has a concept of "internal" agents, keep it: `resolveMode`
  treats `Internal: true` as `subagent` when no explicit `mode` is set
  (`internal/roster/definition.go:49-58`), so legacy defs work untouched.

### 6.2 Existing agent definitions
- `LoadMarkdownDir` **replaces** a YAML agent wholesale when ids collide. If your
  fork has customized YAML entries with ids matching the new md files, either rename
  the md files' ids or fold your customizations into the md. Audit with:
  `grep -h '^id:' config/default/agents/*.md` vs your `agents.yaml` ids.
- Model/provider in the shipped md files (`gpt-oss-120b` / `openrouter`) almost
  certainly differ from your fork's providers — edit the frontmatter, don't hardcode.

### 6.3 Streaming layer (`stream.Encoder`)
- The encoder/pump pair assumes this repo's `internal/stream` interface
  (Text/Reasoning/ToolCall/ToolResult/Agent*/TaskList/Usage/ToolInterrupt/Metadata/
  Progress/Artifact/RunStarted/RunFinished/RunFailed). If your fork's interface is
  smaller, implement only the intersection and drop the unmatched event types from
  the pump switch — the `AgentEvent` schema itself should be ported whole (extra
  event types are harmless; they're just never emitted).
- If your fork has no `ToolInterrupt`/HITL surface, you can still port everything
  except step 5; keep `EvQuestion`/`interrupted` plumbing in place (cheap) so a later
  phase can light it up.

### 6.4 Session service / ADK version
- The task tool calls `session.Service.Create` with a caller-chosen `SessionID` and a
  `State` map, and `runner.New(runner.Config{...})` with `StreamingModeSSE`. Check
  your fork's ADK version (`google.golang.org/adk`); pre-0.5 versions differ in
  `functiontool.New` (Config struct vs positional args) and in
  `toolconfirmation` availability. If `RequireConfirmation` doesn't exist in your ADK
  version, the question tool cannot be ported as-is — gate it out and keep the task
  tool (which only *detects* confirmations in child streams).
- If your fork has its own session-id scheme, keep the `parent:type-shortid`
  **pattern** (parent prefix + `:` separator) — the UI and `/v1/sessions/{id}`
  routing rely on the child id being a single path segment (no `/`).

### 6.5 Handler wiring (biggest risk of stomping local changes)
- Do **not** replace your chat handler. Add `StreamAgentRunBackground` as the new
  path and switch over deliberately: at main, `internal/handler/chat.go:211` calls it
  for the coordinator path. If your fork's chat handler has custom middleware
  (auth, rate limits, request mutation), keep it and only swap the final
  "run the agent and stream" call.
- Route additions are append-only in `internal/server/server.go` — merge them into
  your fork's router (whatever mux it uses). The must-haves for the baseline:
  `GET /v1/sessions`, `GET /v1/sessions/{id}`, `GET /v1/sessions/{id}/stream`,
  `POST /v1/sessions/{id}/cancel`. (`/viewed` is a later phase; skip if not porting
  the ViewedStore.)
- Identity: everything keys on `handler.UserID(r)` (X-User-ID header → "anonymous").
  If your fork has real auth, implement `UserID` over your auth context — it is
  deliberately the single seam.

### 6.6 Later-phase dependencies inside bootstrap
Main's `bootstrap.go` references components from later phases: `NewArchiver`,
`NewCompositeLog`, `SetTaskBoardStore`, `SetViewedStore`, `SetTerminalFlusher`,
post-run hooks (`ArchiveHook`, `TitleHook`, …). For a baseline-only port:
- Use the raw event log directly: `runCoordinator := agent.NewCoordinator(eventLog, logger)`.
- Omit the Set* calls (all optional; nil-safe by design).
- Skip `internal/eventlog/composite.go`, `project.go`, `taskstate.go` and the
  `taskBoards` field in the encoder (guarded by nil checks — either keep the field
  and never set it, or strip it).
- In the coordinator, `nextTurnLocked` uses `eventlog.NextTurn` (project.go — later
  phase). For baseline-only, stub it to return 0 or port `project.go` too (it is
  self-contained and small; porting it is the easier path).

### 6.7 MCP config format collisions
- If your fork already has an `mcp.yaml`/MCP concept (e.g. browser-side MCP), note
  this design is **backend-as-client** — do not merge the two configs; namespace this
  one (the loader path is `config/<env>/mcp.yaml`, `internal/config/mcp.go:69`).
- The manager at main pulls in the OAuth provider even when unused. If you don't
  want the OAuth surface, trim `NewManager` to skip `NewOAuthProvider` and construct
  the remote transport with a plain `http.Client` that injects
  `sc.ExpandedHeaders()` — that preserves H3a static-header auth without the OAuth
  files.

### 6.8 Things NOT to port from the original commit
`git show ab9fb39` contains an `agents/coordinator/` package (a static coordinator
with its own dispatch/checktasks/state files) and `internal/agent/coordinator.go` in
an earlier, buggier form. The static `agents/coordinator` package was **deleted**
later in favor of the dynamic markdown-defined `swarm-coordinator` + task tool. Port
the current-main design only. Similarly, the original memory log closed follow
readers on terminal events — that was the W1 bug; use the main version.

---

## 7. Verification

### 7.1 Build & unit tests

```bash
go build ./... && go vet ./...
go test ./internal/roster/ ./internal/eventlog/ ./internal/tools/ \
        ./internal/agent/ ./internal/mcp/ ./internal/config/
# or the repo-wide targets:
make build && make test
```

Key tests that prove the invariants (names at main):
- `TestMemoryLogSeqMonotonic`, `TestMemoryLogResumeAfterSeq`,
  `TestMemoryLogReplayThenLive`, `TestMemoryLogTerminalBeforeFollowStaysLive`
- `TestReadOnlyPermissionsReproducesLegacyDenySet`, `TestPermissionsLastMatchWins`
- `TestCoordinator_MultiTurn_SecondTurnStreams`, `TestCoordinator_ConcurrentTurn_Queued`,
  `TestCoordinator_Cancel_NoDoubleTerminal`, `TestCoordinator_ErrorRun_StatusError`
- `TestTranslateChildEvent_*` (5 cases), `TestTaskHub_JoinFanIn`,
  `TestTaskHub_TaskListMonotonic`
- `TestQuestionResult_AnswersRoundTripThroughADKPayload`, `TestQuestionResult_Dismissed`
- `TestNewManager_NoPanicOnBadConfig`

### 7.2 Manual smoke — sessions & resume

```bash
# 1. Start the gateway (in-memory event log; no external services needed)
go run ./cmd/server

# 2. Fire a chat turn at the swarm coordinator (adjust agent id to your roster)
curl -N -s http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' -H 'X-User-ID: smoke' \
  -d '{"model":"swarm-coordinator","stream":true,
       "messages":[{"role":"user","content":"Research X and summarize; use two researchers in parallel."}]}'
# Expect: SSE stream with agent-step started/done frames for each dispatched child,
# agent deltas attributed to researcher#<id>, a task-list snapshot, then final text.

# 3. While it runs (or after), list sessions and attach
curl -s -H 'X-User-ID: smoke' http://localhost:8080/v1/sessions | jq
SID=<session id from above>
curl -N -s -H 'X-User-ID: smoke' "http://localhost:8080/v1/sessions/$SID/stream?after=0"
# Expect: byte-identical replay of the whole run, then live tail / heartbeats.
# Kill the first curl mid-run and re-attach with ?after=<last seq> — no gaps, no dupes.

# 4. Invalid resume must 400 (H1): ?after=-1
# 5. Wrong user must 404 (H2): repeat step 3 with X-User-ID: other
# 6. Cancel: POST /v1/sessions/$SID/cancel → stream shows a single cancelled terminal.
```

Question-tool smoke: ask something ambiguous enough that the coordinator calls
`question`; the stream pauses with a question payload (`EvQuestion`), session status
shows `awaiting-input`, then `POST /v1/agent/resume` with
`{"thread_id": SID, "approved": true, "answers": [["Option A"]]}` continues the same
session log.

### 7.3 Manual smoke — MCP / office

```bash
cd services/office-mcp && pip install -r requirements.txt
OFFICE_OUTPUT_DIR=./out python server.py        # :8090/mcp + :8090/files/<name>
# gateway side: config/default/mcp.yaml has office enabled at http://localhost:8090/mcp
# Boot log must show: mcp: server registered  server=office
```

Ask the coordinator to "create a 3-slide deck about Q3 results" → it should call
`create_pptx` (MCP tool, namespaced through the office toolset) and the returned
`http://localhost:8090/files/deck-*.pptx` URL must download a valid file.

Redis-backed log (optional): set `EVENTLOG_STORE=redis` with Valkey configured, rerun
7.2 — behaviour must be identical, and `XRANGE evlog:<app>:<sid> - +` shows the raw
events with ids `<seq>-0`.
