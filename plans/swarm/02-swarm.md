# Phase 02 — Swarm: task tool, child sessions, coordinator

> The headline feature: dynamic dispatch over the typed roster via first-class child sessions, replacing the stringly-typed JSON-board coordinator.

Depends on: 00 (registry, leaf builder, permissions), 01 (sessions, EventLog, run coordinator). Frontend counterpart: cards already largely exist in `agentui` — see [07-frontend-contract.md](07-frontend-contract.md).

## Problem (current state, verified)

`agents/coordinator/` + `agents/swarm/` are a `loopagent.New` of three sub-agents (`coordinator/agent.go:140`): an LLM emitting a JSON board into `OutputKey: coordinator:task_board`; a `dispatch` code-agent parsing the board + fanning workers over goroutines re-authoring events as `worker#taskID` (`dispatch.go:100-128`); a `check_tasks` code-agent deciding done/escalate (`checktasks.go`). The two packages are near-identical. Workers are a **static** `sub_agents` list; the coordinator can't introduce a type at runtime; the roster the LLM sees is a hand-built `<available_workers>` string (`checktasks.go:156`).

## Key adk-go facts (verified, decisive)

- **ADK runs multiple `FunctionCall`s from one model turn SEQUENTIALLY** (`base_flow.go:542`, with `TODO: check feasibility of running tool.Run concurrently` at `:527`). ⇒ parallel `task()` calls in one turn do NOT run concurrently under ADK. Parallelism must live in **our glue**.
- **`AgentTool.Run` hard-codes `session.InMemoryService()`** (`agent_tool.go:167`) + in-memory artifact/memory (`:174-175`) → child is a **throwaway**: not persisted, not streamable, not resumable; only the final text is returned (`:228-249`). Unusable for first-class child sessions.
- `tool.Context` exposes `SessionID()`, `UserID()`, `AppName()`, `InvocationID()`, `State()` — enough to mint + link a child session.
- A foreground function tool runs **inside** the parent runner's tool-execution while the parent iterator is blocked (`base_flow.go:645`) — no encoder race for foreground tasks.

## 1. The `task` tool (`internal/tools/task.go`)

```go
type TaskArgs struct {
    SubagentType string `json:"subagent_type" desc:"One of the available subagent types (see list)."`
    Description  string `json:"description"   desc:"Short 3-7 word label for the UI card."`
    Prompt       string `json:"prompt"        desc:"Full, self-contained instruction for the subagent."`
    Background   bool   `json:"background,omitempty"`
}
type TaskResult struct {
    SessionID string `json:"session_id"` // child session id
    Status    string `json:"status"`     // completed|running|failed
    Result    string `json:"result"`     // child final text; coordinator wraps <task_result>
}
```

Built via `functiontool.New[TaskArgs,TaskResult]` (like `todowrite`). Its `Description` is built at construction from `Registry.Manifest(allowedSubagents)` so the model sees valid `subagent_type`s inline. Closure deps (`TaskDeps`: `*roster.Registry`, `session.Service`, `tools.Deps`, `*config.Config`, the per-request `*RunBus`) captured at build.

**Why our own Runner, not `AgentTool`:** to get a persisted, streamable, resumable child session we run our own `runner.New` (same constructor as `core.go:139`) against the **shared persisted `session.Service`** with a fresh child `sessionID`. This is the central "own the glue" decision.

**Foreground lifecycle:**
1. Resolve `args.SubagentType` against the registry; if absent or not in the caller's `AllowedSubagents`, return `failed` + the valid list (model self-corrects).
2. Mint child session: `childID := parentSessionID + "/" + shortid()`; `session.Service.Create` with metadata `{parentID, subagentType, description}` (parentID under reserved state key `_meta:parentID`).
3. Build child via `roster.Construct(reg, def, overlay, deps)` — leaf by default (no `task` tool). Overlay carries the per-request model override (replaces `AgentConfig` override propagation).
4. Run our Runner: `r.Run(ctx, userID, childID, genai.NewContentFromText(args.Prompt, RoleUser), {SSE})`. Drain the iterator: each event **published to the RunBus tagged `{childID, subagentType, parentID}`** (so the Phase-01 pump attributes it to the child card); accumulate the last non-partial text.
5. Return `TaskResult{childID, "completed", finalText}`. The coordinator's prompt contract wraps it `<task_result subagent_type=… session=…>…</task_result>` (opencode parity).

For pipeline-typed children, collect the declared output (last text vs `OutputKey`), not blindly the last text — else `<task_result>` could be empty.

## 2. Parallelism — `background` + `task_join` (recommended)

Since ADK can't run parallel tool calls, keep concurrency in our glue:
- `task(background:true)` spawns the child on a goroutine writing to the RunBus and returns immediately `{session_id, status:"running"}`.
- A companion `task_join(session_ids:[…])` tool blocks until those children finish and returns their `<task_result>`s. Fan-out/fan-in under explicit model control (the opencode mental model). Each child = its own persisted session + card. Bound concurrency with a `maxParallelWorkers` semaphore (carried from `Definition`/config, replacing `MaxParallelWorkers`).
- **Optional** `task_batch(tasks:[…])` builds a transient `parallelagent.New` over freshly-`Construct`ed children under one child Runner — only for genuinely-atomic batches (tradeoff: one shared session, interleaved tokens, weaker per-child card isolation, no independent resume). Prefer `background`+`join`.
- **Do NOT** try to make ADK execute parallel `FunctionCall`s concurrently (upstream change `:527`, breaks sequential state-delta assumptions).

## 3. Gated nesting

A child is a leaf unless `Definition.CanDispatch == true`, in which case `roster.Construct` injects `task` (scoped to its own `AllowedSubagents`), making it a sub-coordinator with grandchildren (parentID chains naturally). Guard with a depth cap (`_meta:depth`, default max 2) + a per-run total-spawn cap — the model controls spawning.

## 4. The coordinator = an ordinary `LlmAgent`

Built by the same leaf builder (Phase 00); its `Definition` has `CanDispatch:true` + an `AllowedSubagents` list. Tools: `task`, `task_join` (and/or `task_batch`), `todowrite`, `emit_artifact`. The `<available_subagents>` manifest is in its prompt + the task tool description.

**The loop = the LLM's own tool-calling loop**, which ADK's `Flow` already drives: emit `task(...)` → ADK executes → `<task_result>` returns into context → spawn more / `todowrite` / synthesize. Termination is natural (model stops emitting tool calls). **Delete:** the JSON board contract (`coordinator/agent.go:87-104`), `dispatch.go`, `checktasks.go`, `state.go`, `tasks.go`, the `loopagent.New` wrapper, the entire `agents/swarm/` mirror. The `MaxIterations` churn-guard disappears (no re-emitted board to churn on). Safety: coordinator `MaxSteps` + depth cap.

The coordinator **is** the output agent (`Core.OutputAgent` = coordinator name); `isOutputAgent` routes its tokens to `enc.Text`, children to `enc.AgentText`/`AgentDelta`.

## 5. RunBus (`internal/agent/runbus.go`) — per-request fan-in

The encoder is not concurrency-safe and has a single consumer; a background child runs on its own goroutine while the parent loop produces → two producers, one encoder → needs serialization.

```go
type AttributedEvent struct { SessionID, SubagentType, ParentID string; Event *session.Event }
type RunBus struct { ch chan AttributedEvent /* + ref-count of live producers */ }
```

- Producer 1 (coordinator): a goroutine drains `core.Runner.Run` and publishes tagged `{root threadID, ""}`.
- Producer N (children): the `task` handler publishes tagged `{childID, subagentType, parentID}`.
- Single consumer: the Phase-01 pump ranges over `bus.ch`. The disciplined generalization of today's ad-hoc `chan *session.Event` + `worker#taskID` re-authoring — now children are real sessions.

## 6. Wire / UI attribution (opencode parity)

- The coordinator's `task` tool-call surfaces as a normal main-thread tool call; extend its emitted metadata to carry `childSessionId`+`subagentType` (the opencode `ToolPart.metadata.sessionId` link). Child card key = `subagentType` (matches "cards keyed by type"); disambiguate multiple instances of one type by child session id (the `worker#taskID` role today).
- Child tokens/steps flow through existing attributed encoder methods: `AgentStart/Done`→`data-agent-step`; `AgentText/Reasoning`→`data-agent-delta{agent,kind,delta}`; `AgentProgress`→`data-agent-progress`. `agent` field = `subagentType`.
- **Task list** (`data-task-list`, full-snapshot): two writers unified in `internal/tasks` — (a) `todowrite`→`tasks.MapTodos` (unchanged); (b) **synthesised from `task()` spawns** — replace `BoardFromStateDelta` (reads the deleted `:task_board` key) with a `TaskList` maintained by the RunBus (each spawn appends `{id:childID, title:description, status, agent:subagentType}`, status updated on child completion). Reuse `tasks.Clamp`/`Signature`. This merged structure is the Redis-typed per-session todo state, re-emitted as a full snapshot.
- **Resumability:** each child is a persisted session, so a card offers open/resume via the Phase-01 `/v1/sessions/{childID}/stream`. HITL inside a child pauses its **own** session.

## 7. Where ADK still helps

- `AgentTool` for synchronous ephemeral "consult a specialist" (no card/session) — its throwaway in-memory session is acceptable there. **Rule: `task` = first-class child session + card; `AgentTool` = ephemeral function call.**
- `Sequential`/`Parallel`/`Loop` behind `PipelineSpec` (Phase 00) for static author-defined DAGs (`deepresearch`, `triage`). **Rule: dynamic model-chosen fan-out → our `task` dispatch; fixed author-defined DAG → ADK workflow.**

## Files

**Add:** `internal/tools/task.go` (`task`, `task_join`, optional `task_batch`, `TaskDeps`); `internal/agent/runbus.go`; `config/<env>/agents/coordinator.md`.
**Modify:** `agents/shared/shared.go`; `internal/tools/registry.go` (register task tools + wire `TaskDeps` via closure, like memory tools); `internal/agent/{stream,core}.go` (consume RunBus; OutputAgent = coordinator; SubAgentNames from `Dispatchable`); `internal/tasks/tasks.go` (spawn-synthesised list; keep `MapTodos`/`Clamp`/`Signature`/`statusToUI`); `internal/bootstrap/bootstrap.go`; `internal/handler/chat.go` (override via `roster.Construct`+`Overlay`).
**Delete:** `agents/coordinator/` + `agents/swarm/` (entire packages).
**Keep:** `agents/deepresearch`, `agents/triage` (via `PipelineSpec` or bespoke `Construct` initially), `agents/basic`.

## Risks / open questions

1. **Sequential tool execution** — confirmed; parallelism via background+join. Open: make even single tasks background-capable to avoid the foreground block; coordinator prompt prefers background+join for >1 spawn.
2. **Child Runner vs adk state scoping** — child gets fresh session-scoped state but shares `app:`/`user:` with parent. Seed the child with a *snapshot* of relevant parent state (mirror `agent_tool.go:181-188`), don't alias, so a child can't corrupt the parent log. Confirm the Valkey session service keys `user:` state by `userID` not `sessionID` (so child `add_memory` is visible to parent — likely desired).
3. **LoopAgent escalation propagation** (`deepresearch/agent.go:35`) — encoded in the `PipelineSpec` interpreter (Phase 00); reject/lower mid-sequence loops at load.
4. **Encoder concurrency** — RunBus single-consumer is load-bearing; no code writes the encoder off the consumer goroutine. HITL mid-stream return must drain/cancel outstanding background children (ref-count + context cancel) to avoid orphaned producers.
5. **Unbounded nesting / spawn storms** — depth cap + per-run total-spawn cap + `maxParallelWorkers`. Open: global vs per-subtree token/step budget when grandchildren spawn.
6. **HITL inside a child** — must resume on the **child** session/runner; key `hitl.PendingInterrupt` by child id (currently threadID).
7. **Final-text fidelity for pipeline children** — consult the child Definition's declared output (text vs `OutputKey`).
8. **`GET /v1/agents` shape** — `sub_agents` now = dispatchable roster; coordinate with agentui.

## Verification

A deep-research prompt makes the coordinator emit `task(researcher/...)` → each appears as a typed child card; parallel `task(background)`+`task_join` runs concurrently (check timing); `<task_result>` returns to the coordinator; final synthesis streams to the main thread; task list reflects spawns. A gated sub-coordinator nests one level; depth cap holds. Unit tests: `task` child-session lifecycle; RunBus attribution; spawn-synthesised task list.
