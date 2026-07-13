# 03 — Swarm Visibility, Parallelism & Governance (W3)

**Source commits (merged main @ `17b1a87`):**
- `ee0e1c4` — "W3: swarm visibility, parallelism, governance" (+814 / −1487 lines)
- `3746c2b` — "W3 follow-up: exclude mode:subagent agents from GET /v1/agents"

**Prerequisites:** docs 01–02 (durable event log with `eventlog.AgentEvent` / `eventlog.EventLog`,
run coordinator, run framing / pump / aisdk encoder). This doc assumes the fork already has
`eventlog.Append` (mutex-safe, concurrent-writer-safe), the pump that projects `AgentEvent`s to
AI-SDK stream parts, and the `roster.Registry` typed view over agent configs. If the fork's
roster/registry work landed in an earlier doc, only extend it here; do not duplicate.

**Audience note:** you are porting into a **diverged fork**. Read each section, find the fork's
equivalent, and adapt. Do not blind-copy files — the upstream excerpts below are the *contract*;
your implementation must satisfy the contract while preserving the fork's local changes
(different package names, extra tools, different session-service wiring are all fine).

---

## 1. Purpose & architecture

Before W3 the repo had **two static swarm implementations**:

- `agents/coordinator/` (~600 lines): a `LoopAgent` tree — `coordinator` LLM writes a JSON task
  board into session state (`OutputKey`), a `dispatch` code-agent parses the board and runs
  workers, a `check_tasks` code-agent decides done-vs-loop, bounded by `MaxIterations`.
- `agents/swarm/` (~630 lines): the same JSON-board loop pattern with a different prompt and
  key names (`swarm:task_board`, `swarm:synthesis`).

Both were deleted. Their problems:

1. **Static topology.** Workers had to be pre-declared as `sub_agents` in YAML; the LLM could
   not choose an arbitrary specialist at runtime.
2. **Fragile control flow.** "Is the coordinator's text a JSON array or a synthesis?" —
   dispatch decisions parsed from free-form model output.
3. **Invisible children.** Worker output went into session state; the user saw nothing until
   the loop escaped. No live streaming, no per-worker attribution.
4. **Duplicate maintenance.** Two ~600-line copies of the same loop.

W3 replaces both with **dynamic task-tool dispatch**:

- A coordinator is now just a **leaf LLM agent** (`type: basic`) whose tool list contains
  `task` (and usually `task_join`). No special builder, no LoopAgent.
- The `task` tool resolves a `subagent_type` against the typed roster registry, builds a child
  agent on the fly, mints a **first-class child session** (`parentID:type-shortid`, reachable at
  `/v1/sessions/{id}`), and runs it via its own ADK `Runner` with `StreamingModeSSE`.
- Every child event is translated (`translateChildEvent`, a pure function) into parent-log
  `AgentEvent`s attributed to `"<type>#<shortid>"` and appended to the **parent** session's
  event log — so the parent UI renders live sub-agent cards while children run.
- `background:true` spawns the child on a goroutine and returns `{session_id, status:"running"}`
  immediately; `task_join(session_ids)` blocks and fans in the results. A **TaskHub** (shared
  per-coordinator between task + task_join) tracks the jobs and bounds concurrency with a
  semaphore (default 4).
- The hub also synthesises a **task list**: one row per spawned child
  (`{id,title,status,agent}`), emitted as monotonic `EvTaskList` snapshots on spawn and on
  completion (settled statuses never regress).
- **Governance:** an agent definition may declare `allowed_subagents`; a per-coordinator
  `TaskFactory` builds a governed `(task, task_join)` pair whose manifest and dispatch check
  are restricted to that allowlist.
- **Agents as markdown:** curated subagent definitions live in `config/<env>/agents/*.md`
  (YAML frontmatter + markdown body as the system prompt), merged over `agents.yaml` by id.
  `mode: subagent` entries are spawn-only; `mode: all` entries are both user-selectable and
  dispatchable.
- `GET /v1/agents` excludes `mode: subagent` entries (follow-up `3746c2b`).

### If the fork still has static coordinator/swarm implementations — delete them

Search the fork for equivalents of `agents/coordinator/`, `agents/swarm/`, JSON-task-board
loop agents, `check_tasks`/`dispatch` code-agents, and their YAML entries
(`task-coordinator`, `research-swarm` upstream) and **remove them** as part of this port.
Keeping them alongside dynamic dispatch causes: duplicate `can_dispatch` semantics, roster-id
collisions with the new subagent definitions, and two divergent swarm UIs. Upstream also had
to rename two deep-research pipeline workers whose ids collided with the new subagent ids
(`data-analyst` → `dr-data-analyst`, `gap-analyst` → `dr-gap-analyst`) — check the fork for
the same collisions before adding the new markdown defs.

---

## 2. Dispatch sequence (ASCII)

```
 user                parent run              task tool                    child runner
  │                     │                       │                             │
  │  "research X and Y" │                       │                             │
  ├────────────────────►│                       │                             │
  │                     │ tool_call task(       │                             │
  │                     │  subagent_type=       │                             │
  │                     │  researcher,          │                             │
  │                     │  background=true) ───►│                             │
  │                     │                       │ Registry.Get + isAllowed    │
  │                     │                       │ BuildChild(def)             │
  │                     │                       │ childID = parent:researcher-a1b2c3d4
  │                     │                       │ Hub.register(childID)       │
  │                     │                       │ go { sem <- ; run } ───────►│ Create child session
  │                     │◄── {session_id,       │                             │ (state: _meta:parentID,
  │                     │     status:"running"} │                             │  _meta:subagentType, …)
  │                     │                       │                             │
  │                     │      PARENT EVENT LOG (concurrent appends)          │
  │                     │  ◄── EvAgentStep{KindStarted, Author:"researcher#a1b2c3d4"}
  │                     │  ◄── EvTaskList{[{id,title,status:"running",agent}]}│
  │                     │                       │      r.Run(SSE) events ◄────┤
  │                     │  ◄── EvAgentDelta{KindReasoning|KindText} … (live)  │
  │                     │  ◄── EvToolCall / EvToolResult (not IsOutput)       │
  │                     │  ◄── EvAgentStep{KindDone, Duration}                │
  │                     │  ◄── EvTaskList{[{…status:"completed"}]}            │
  │                     │                       │ Hub.finish(childID, result) │
  │                     │ tool_call task_join(  │                             │
  │                     │  [childID, …]) ──────►│ Hub.wait(ctx, id) blocks    │
  │                     │◄── {results:[{session_id,status,result}, …]}        │
  │                     │ synthesise final answer                             │
  │◄────────────────────┤                       │                             │
  │                     │                       │                             │
  │   (SSE view: pump reads parent log → data-agent-step / data-agent-delta / │
  │    data-task-list parts; reload: ProjectMessages folds the same events)   │
```

Foreground (`background:false`) is the same minus register/finish/join: the tool call itself
blocks on the semaphore, runs the child with a context **derived from the tool context**
(cancelling the parent cancels the child), and returns the final `TaskResult` directly.

---

## 3. File inventory

Upstream paths — map each to the fork's layout.

**New files**

| Path | Role |
| --- | --- |
| `internal/tools/child_stream.go` | `childStreamState`, `translateChildEvent` (pure), `childLogSink` |
| `internal/tools/task_test.go` | translate + hub unit tests (list in §7) |
| `config/default/agents/researcher.md` | `mode: subagent` — web/KB/Confluence research |
| `config/default/agents/data-analyst.md` | `mode: subagent` — query/analysis specialist |
| `config/default/agents/gap-analyst.md` | `mode: subagent` — coverage/gap review |
| `config/default/agents/report-writer.md` | `mode: subagent` — synthesis/writing |
| `config/default/agents/swarm-coordinator.md` | the dynamic coordinator (type: basic, tools: task/task_join/todowrite/…, `allowed_subagents`) |

**Rewritten / extended**

| Path | Change |
| --- | --- |
| `internal/tools/task.go` | full rewrite: `TaskArgs` (+`Background`), `TaskDeps`, `TaskHub`, `NewTaskTool`, `runChild`, `NewTaskJoinTool` |
| `internal/tools/registry.go` | register `task_join`; `Deps` gains `TaskTool`, `TaskJoinTool`, `TaskFactory` |
| `internal/bootstrap/bootstrap.go` | `buildChild` closure (deny question + task/task_join for leaves), `deps.TaskFactory`, ungoverned fallback, markdown-agents load, drop coordinator/swarm builders |
| `internal/config/agents.go` | `AllowedSubagents []string` yaml field (+ Clone) |
| `internal/roster/definition.go` | `CanDispatch` derived from `slices.Contains(ac.Tools, "task")` (dead `dispatchTypes` map removed); `AllowedSubagents` on Definition |
| `internal/roster/load_markdown.go` | frontmatter gains `allowed_subagents` |
| `agents/shared/shared.go` | governed TaskFactory swap-in inside `BuildLLMAgent` |
| `config/default/agents.yaml` | −203 lines: `task-coordinator`, `research-swarm`, their board workers removed; deep-research worker renames |
| `internal/handler/models.go` | (follow-up) `GET /v1/agents` filters `a.Internal || a.Mode == "subagent"` |

**Deleted (do the same in the fork if equivalents exist)**

```
agents/coordinator/{agent,checktasks,dispatch,state,tasks}.go   (~600 lines)
agents/swarm/{agent,coordinator,dispatch,state,tasks}.go        (~630 lines)
```

---

## 4. Step-by-step implementation

### Step 1 — Roster: mode, CanDispatch, AllowedSubagents

1. Ensure `roster.Mode` exists with `primary | subagent | all` and that
   `resolveMode` maps an explicit `mode:` first, then legacy `Internal → subagent`,
   default `primary`.
2. In the Definition constructor, derive `CanDispatch` from the tool list — **not** from an
   agent-type map:

   ```go
   CanDispatch: slices.Contains(ac.Tools, "task"),
   ```

3. Add `AllowedSubagents []string` to the config DTO (`yaml:"allowed_subagents,omitempty"`,
   copied in `Clone()`), to the markdown frontmatter struct, and to `Definition`.
4. Registry API needed by the task tool: `Get(name)`, `Dispatchable()` (mode subagent|all),
   `Primary()` (mode primary|all), and `Manifest(allowed []string) string` which renders a
   deterministic (sorted) `<available_subagents>` block of `- name: description` lines,
   restricted to `allowed` when non-empty.

**Checkpoint:** unit-test `Manifest(nil)` lists every dispatchable def and
`Manifest([]string{"researcher"})` lists only it. `go build ./...` green.

### Step 2 — child_stream.go (the translation layer)

Create the fork's equivalent of `/Users/sri/code/agentic/internal/tools/child_stream.go`.
This is the heart of swarm visibility; port it faithfully. Contract in §5. Key rules:

- **Pure function**: `translateChildEvent(ev, label, subagentType, childID, step, st)` does no
  I/O; every emitted event gets `Author=label`, `SubagentType`, `SessionID=childID`, `Step=step`
  stamped on.
- Partial events → `EvAgentDelta` per non-empty text part: `KindReasoning` if `p.Thought`,
  else `KindText` (and set `st.hadPartial = true` for text).
- Non-partial events: reasoning parts still emit `KindReasoning` deltas; text parts are
  **joined** into one `msgText` (fixes the old Reset-per-part truncation) and only emitted as
  deltas if `!st.hadPartial` (no double-emit after streaming). Non-empty `msgText` overwrites
  `st.finalText` ("last full message wins"). Reset `st.hadPartial = false` at the end of every
  non-partial event.
- FunctionCall named `toolconfirmation.FunctionCallName` (`adk_request_confirmation`) →
  set `st.blocked = true`, emit nothing (child HITL has no resume path here; the caller turns
  this into a clear failure instead of a deadlock). Other FunctionCalls → `EvToolCall` with
  `ToolPayload{ID, Name, Args: json}`; FunctionResponses (except confirmation) → `EvToolResult`.
  These are appended **without `IsOutput`**, so the pump routes them to
  `AgentToolCall`/`AgentToolResult` (upstream's aisdk encoder renders those as no-ops /
  progress, NOT as main-thread `tool-input-*` parts — if the fork's encoder renders
  main-thread tool parts for child tools, child tool calls will pollute the parent transcript).
- `childLogSink` wraps `eventlog.Append` with `ev.V = 1` and a `context.WithoutCancel` context
  (a done/cancelled parent must still be able to record the child's terminal events).

**Checkpoint:** port the six translate tests (§7) and get them green before touching task.go.

### Step 3 — task.go rewrite

Port `/Users/sri/code/agentic/internal/tools/task.go`. Components:

1. **`TaskArgs`** — `subagent_type`, `description` (3–7 word UI card title), `prompt`
   (self-contained; child does not see the parent conversation), `background` (bool).
2. **`TaskResult`** — `{session_id, status: completed|running|failed, result}`. The
   coordinator prompt contract wraps results in `<task_result>`.
3. **`TaskDeps`** — registry, app name, session service, **parent** event log, `BuildChild`
   func (injected from bootstrap to avoid an import cycle with agent builders), `Allowed`,
   `MaxParallel`, `Hub`.
4. **`TaskHub`** — mutex + `jobs map[string]*taskJob` (done-chan + result, keyed by child
   session id) + `tasks map[string][]eventlog.TaskItem` (spawn-synthesised list keyed by
   **parent** session id) + `sem chan struct{}` (cap = `MaxParallel`, default 4).
   Methods: `register`, `finish` (idempotent-ish: nil-check the job), `wait(ctx,id)`
   (select on done vs ctx; on ctx-cancel return `status:"running"` with a note),
   `upsertTask` (update-by-ID, settled statuses `done|error|completed|failed` never regress
   to running, returns a **copy** snapshot).
5. **`NewTaskTool`** — description embeds `Registry.Manifest(d.Allowed)`. Handler:
   - resolve def; reject unknown/not-allowed/nil-config with a `failed` TaskResult listing
     valid types (return the error **in-band**, not as a Go error — the model must read it).
   - `BuildChild(def)`; build a fresh `runner.New` against the **shared** session service.
   - `childID = parentID + ":" + subagentType + "-" + uuid[:8]` — **`:` not `/`** so the child
     session id is mux-safe and reachable via `GET /v1/sessions/{id}` (upstream "M5" fix).
     `label = subagentType + "#" + short` for multi-instance disambiguation.
   - background: `Hub.register(childID)`, goroutine acquires the semaphore, runs with
     `context.WithoutCancel(tc)` (child outlives the tool call), `Hub.finish`. Returns
     `{session_id, status:"running"}` immediately.
   - foreground: acquire semaphore, run with `tc` itself (parent cancel → child cancel).
6. **`runChild`** — the sequence in §2's diagram: create child session seeded with
   `_meta:parentID`, `_meta:subagentType`, `_meta:description` state (tolerate
   "already exists"); compute `step` from the parent log head (`Head()+1`, fallback 1);
   append `EvAgentStep{KindStarted}`; upsert task row `status:"running"` + emit `EvTaskList`
   snapshot; iterate `r.Run(ctx, userID, childID, content, RunConfig{StreamingModeSSE})`
   feeding `translateChildEvent` → sink; append `EvAgentStep{KindDone, Duration: ms}`;
   final task-list snapshot with `status: "completed"` on success or `"error"` on
   runErr/blocked. **Use `"completed"`, not `"done"`** — upstream's UI TaskBar counts
   `status === "completed"` as finished (check what *your fork's* task bar counts and match
   it). Blocked children return `status:"failed"` with the explain-and-retry note plus
   `st.finalText` as partial output.
7. **`NewTaskJoinTool`** — takes the shared hub; for each id `hub.wait(tc, id)`;
   unknown id → in-band failed result.

**Checkpoint:** `TestTaskHub_JoinFanIn` + `TestTaskHub_TaskListMonotonic` green;
`go vet ./...` clean.

### Step 4 — registry + bootstrap wiring

1. `internal/tools/registry.go`: add `task_join` to the known-tool list; `Deps` gains
   `TaskTool`, `TaskJoinTool` (shared fallbacks) and
   `TaskFactory func(allowed []string) (task, join tool.Tool, err error)`. `ResolveTools`
   returns `deps.TaskTool` / `deps.TaskJoinTool` for those names.
2. `internal/bootstrap/bootstrap.go`:
   - load markdown agent defs after YAML:
     `roster.LoadMarkdownDir(agentsCfg, filepath.Join(configDir, "agents"))` (missing dir OK);
     build `reg := roster.FromAgentsConfig(agentsCfg)`.
   - `buildChild` closure: clone the def's config; if `!def.CanDispatch`, filter out
     `task`, `task_join`; **always** filter out `question` (no HITL resume path for spawned
     children — this is the deadlock guard); build via the shared leaf builder. An agent whose
     own tool list includes `task` keeps dispatch — *gated nesting*.
   - `deps.TaskFactory` mints a fresh `TaskHub` per coordinator and builds the governed pair
     with `Allowed: allowed`.
   - keep an ungoverned shared pair as fallback (`deps.TaskFactory(nil)` →
     `deps.TaskTool/TaskJoinTool`) — see Known issue (c) before you copy this verbatim.
   - **delete** the `"coordinator"` and `"swarm"` entries from the builders map and their
     imports.
3. `agents/shared/shared.go` (`BuildLLMAgent`): when `deps.TaskFactory != nil` and the tool
   list contains `task` or `task_join`, call the factory with `agentCfg.AllowedSubagents`
   and swap the returned pair into a **copy** of deps before `ResolveTools` — see Known
   issue (c) about the silent `err == nil` swallow.

**Checkpoint:** server boots; startup log shows the roster (primary/dispatchable counts) and
"task dispatch + join tools ready"; no references to the deleted packages remain
(`grep -rn "agents/coordinator\|agents/swarm" --include='*.go'` → empty).

### Step 5 — agent definitions (markdown)

Author the five defs under the fork's config agents dir. Frontmatter contract
(see `/Users/sri/code/agentic/config/default/agents/researcher.md` and
`swarm-coordinator.md` for full examples):

```markdown
---
id: researcher
mode: subagent            # spawn-only; never in GET /v1/agents
name: researcher
description: Researches a focused question ... returning sourced findings.
model: <fork's model>
provider: <fork's provider>
tools: [web_search, retrieve_documents, ...]
---
You are a researcher subagent. You are given a single, self-contained task...
```

The coordinator def: `type: basic`, `tools: [task, task_join, todowrite, emit_artifact,
question]`, `allowed_subagents: [researcher, data-analyst, gap-analyst, report-writer,
explore-agent, plan-agent]` (adjust ids to the fork's roster; make explore/plan `mode: all`
if the fork wants them dispatchable). Its prompt teaches the dispatch protocol: plan with
todowrite → `task(...)` per subtask, `background:true` + `task_join` for independent
subtasks → synthesise, don't concatenate. Note the **description quality matters**: the
manifest line `- name: description` is all the coordinator sees when picking a type.

**Checkpoint:** `GET /v1/agents` lists the coordinator but none of the `mode: subagent` defs;
the task tool's description (log it or inspect via a debug endpoint) shows exactly the
allowlisted six.

### Step 6 — GET /v1/agents mode filter (follow-up 3746c2b)

In the fork's agents-listing handler, extend the filter:

```go
if a.Internal || a.Mode == "subagent" {
    continue
}
```

**Checkpoint:** see Step 5's checkpoint; also confirm `mode: all` agents still list.

---

## 5. Key contract — translateChildEvent → frontend parts

This is the exact chain the frontend consumes. Do not rename event kinds or part types
without checking the fork's UI.

**Child ADK event → parent-log AgentEvent** (all stamped `Author="type#shortid"`,
`SubagentType`, `SessionID=childID`, `Step`):

| Child event | Parent-log event |
| --- | --- |
| `Partial`, text part, `Thought` | `EvAgentDelta{Kind: KindReasoning, Text}` |
| `Partial`, text part | `EvAgentDelta{Kind: KindText, Text}` (+ `hadPartial=true`) |
| non-partial, `Thought` text | `EvAgentDelta{Kind: KindReasoning, Text}` |
| non-partial text (no prior partials) | `EvAgentDelta{Kind: KindText, Text}` |
| non-partial text (after partials) | *(nothing — already streamed; text still folds into finalText)* |
| `FunctionCall` = adk_request_confirmation | *(nothing; `st.blocked = true`)* |
| other `FunctionCall` | `EvToolCall{Tool: {ID, Name, Args}}` — **no IsOutput** |
| `FunctionResponse` (non-confirmation) | `EvToolResult{Tool: {ID, Name, Result}}` — **no IsOutput** |

Plus, emitted by `runChild` around the stream:
`EvAgentStep{KindStarted}` … `EvAgentStep{KindDone, Duration(ms)}` and `EvTaskList{Tasks}`
snapshots (rows: `TaskItem{ID: childID, Title: description, Status, Agent: subagentType}`).

**Parent-log event → SSE part** (live: `internal/agent/pump.go` →
`internal/stream/aisdk/encoder.go`; reload: `internal/eventlog/project.go` produces the same
part types with `PartAgentStep` / `PartAgentDelta` / `PartTaskList` constants):

```jsonc
// EvAgentStep KindStarted / KindDone  →  id "<agent>-<step>" is STABLE per step
{"type":"data-agent-step","id":"researcher#a1b2c3d4-7",
 "data":{"agent":"researcher#a1b2c3d4","step":7,"status":"started"}}
{"type":"data-agent-step","id":"researcher#a1b2c3d4-7",
 "data":{"agent":"researcher#a1b2c3d4","step":7,"status":"done","durationMs":8231}}

// EvAgentDelta KindText / KindReasoning
{"type":"data-agent-delta","id":"adelta-42",
 "data":{"agent":"researcher#a1b2c3d4","step":7,"kind":"text","delta":"Paris is..."}}
{"type":"data-agent-delta","id":"adelta-43",
 "data":{"agent":"researcher#a1b2c3d4","step":7,"kind":"reasoning","delta":"I should..."}}

// EvTaskList — single stable id "tasks", full snapshot each time, never null tasks
{"type":"data-task-list","id":"tasks",
 "data":{"tasks":[{"id":"<parent>:researcher-a1b2c3d4","title":"Research X",
                   "status":"running","agent":"researcher"}]}}

// child EvToolCall/EvToolResult (Author set, !IsOutput) → AgentToolCall/AgentToolResult
// — upstream encoder: intentional no-ops (activity surfaces via data-agent-progress);
//   they must NOT become main-thread tool-input-*/tool-output-* parts.

// run-level / attributed progress (existing from doc 02)
{"type":"data-agent-progress","id":"prog-3","data":{"phase":"...","message":"...","agent":"..."}}
```

The `agent` key doubles as the UI grouping key — the `type#shortid` namespacing is what lets
two concurrent `researcher` children render as separate cards. `SubagentType` + `SessionID`
on the events give the UI the card→child-session link (`/v1/sessions/{childID}`), which is
why childID must be URL-path-safe (`:` separator).

---

## 6. Known issues — fix during port

Upstream shipped these bugs; the fork must land the port with them **fixed**.

**(a) `can_dispatch` in models.go keyed on deleted types — always false.**
`/Users/sri/code/agentic/internal/handler/models.go` `buildAgentEntry` still has:
```go
canDispatch := a.Type == "coordinator" || a.Type == "swarm"
```
Both types were deleted in this very commit, so the API reports `can_dispatch: false` for
every agent, including swarm-coordinator. Fix: derive from the tool list / roster, e.g.
`canDispatch := slices.Contains(a.Tools, "task")` (matching `roster.Definition.CanDispatch`),
or pass the roster into the handler and read `def.CanDispatch`.

**(b) `mode: subagent` agents are only hidden, not blocked.**
The follow-up filtered them out of `GET /v1/agents`, but bootstrap's build loop skips only
`ac.Internal` — subagent-mode entries (researcher etc.) are still built into the top-level
`Agents` map and remain directly invokable by `agent_id`/`model` through the chat handler.
Fix at build/dispatch, not just listing: in the bootstrap loop skip
`ac.Internal || roster mode == subagent` (keep them in cfg for the task tool / sub-agent
resolution), and/or have the chat handler reject an agent whose roster mode is `subagent`.
Decide once, enforce in one place, and add a test that POSTing chat with
`agent_id=researcher` is rejected.

**(c) TaskFactory error silently falls back to the UNGOVERNED shared task tool.**
In `agents/shared/shared.go`:
```go
task, join, err := deps.TaskFactory(agentCfg.AllowedSubagents)
if err == nil { /* swap in governed pair */ }
// err != nil → falls through to deps.TaskTool (allowed = nil = ALL dispatchable)
```
Governance fails **open**: a factory error hands the coordinator the everything-allowed tool
with no trace. Fix: fail closed — return the error from `BuildLLMAgent` (preferred; the agent
should not boot with wrong permissions), or at minimum log at error level and drop
`task`/`task_join` from the tool list rather than substituting the ungoverned pair. Apply the
same scrutiny to the bootstrap fallback (`deps.TaskFactory(nil)` populating shared
`TaskTool/TaskJoinTool`): it exists for agents with `task` but no factory path; make sure the
fork can't reach it for an agent that declared `allowed_subagents`.

**(d) Foreground semaphore acquire has no ctx select.**
`NewTaskTool` foreground path:
```go
d.Hub.sem <- struct{}{}          // blocks forever if the hub is saturated
defer func() { <-d.Hub.sem }()
return run(tc), nil
```
(The background goroutine has the same bare acquire.) If one swarm saturates the hub with
long background children, a foreground dispatch blocks with **no cancellation** — cancelling
the parent run does not release the waiting tool call. Fix both acquires:
```go
select {
case d.Hub.sem <- struct{}{}:
case <-tc.Done():   // background path: parent ctx before WithoutCancel is taken
    return TaskResult{Status: "failed", Result: "dispatch cancelled while waiting for a worker slot"}, nil
}
```
(For the background goroutine, capture cancellation intent before entering
`context.WithoutCancel`; a cancelled-before-start child should `Hub.finish` with a failed
result so `task_join` doesn't wait forever.)

**(e) TaskHub jobs/tasks maps are never pruned.**
`h.jobs` entries and `h.tasks[parentID]` rows live for the life of the hub (which lives as
long as the built coordinator agent — effectively the process). Every dispatch leaks a
`taskJob` + a `TaskItem` row. Fix: evict on terminal — e.g. in `finish`, keep the job but
schedule removal after a grace period (task_join must still find recently-finished jobs;
a `time.AfterFunc(retention, delete)` or a joined-flag sweep is fine), and clear
`h.tasks[parentID]` when its last row settles (or when the parent run terminates, if the hub
can observe that). Bound the cost: the important property is that a long-lived process
doesn't grow without bound.

**(f) gofmt.**
Several W3-touched files were not gofmt-clean in the original commit; on current main
`gofmt -l` still flags `internal/tools/task.go`, `internal/tools/registry.go`,
`internal/bootstrap/bootstrap.go`, `internal/config/agents.go` (e.g. the aligned
`Hub    *TaskHub` field comment block in task.go). Run `gofmt -w` (or `goimports -w`) over
every file you touch in the fork and add a CI/`go vet`+`gofmt -l` check if the fork lacks one.

---

## 7. Fork-adaptation notes

- **Event-log dependency, not stream dependency.** Children write to the parent's *durable
  log*; visibility falls out of docs 01–02's pump. If the fork's pump/encoder names differ,
  the invariants to preserve are: (1) child tool events carry `Author` and no `IsOutput`;
  (2) `data-agent-step` part id is `"<agent>-<step>"` and stable across started→done;
  (3) `data-task-list` uses the single id `"tasks"` with full snapshots; (4) the reload
  projector (`ProjectMessages`) folds the same events into the same part shapes.
- **`tc.SessionID()` must be the parent's session id.** If the fork's tool-context plumbing
  differs (e.g. wraps sessions), verify the id used for `parentID` matches the id the event
  log and `/v1/sessions/{id}` use, or child events will land in the wrong log.
- **Child session creation:** upstream's ADK runner requires the session to exist before
  `Run`. If the fork's runner auto-creates sessions, keep the explicit `Create` anyway for
  the `_meta:*` seed state (parent link, subagent type, description) — the sessions UI uses it.
- **BuildChild injection:** it exists to break the tools↔agent-builders import cycle. If the
  fork has no cycle, you may still keep the closure — it is also where the
  question/task/task_join deny-list lives.
- **Semaphore scope:** upstream bounds concurrency per coordinator *instance* (hub minted per
  TaskFactory call at build time). If the fork builds agents per-request, that becomes
  per-request; decide whether you want a process-global bound instead and say so in a comment.
- **Model/provider fields** in the .md defs are upstream's (`gpt-oss-120b`/`openrouter`);
  substitute the fork's models. Same for `mcp_servers: [office]` on the coordinator — upstream
  later removed the office dependency from swarm-coordinator; include MCP servers only if the
  fork has them.
- **Name collisions:** before adding researcher/data-analyst/gap-analyst/report-writer ids,
  grep the fork's roster for existing agents with those ids (upstream had to rename two
  deep-research workers to `dr-*`).
- **Do not port the deleted code.** If the fork previously merged upstream's
  coordinator/swarm packages (JSON board + LoopAgent), delete them and their YAML entries in
  this step; if the fork has its *own* static swarm, migrate its prompt content into the
  coordinator .md and delete the machinery.

---

## 8. Verification

### Unit tests (port from `/Users/sri/code/agentic/internal/tools/task_test.go`)

```
TestTranslateChildEvent_PartialTextToAgentDelta        # partial text → EvAgentDelta KindText, stamped author/step/ids
TestTranslateChildEvent_ReasoningKind                  # Thought parts → KindReasoning (partial + non-partial)
TestTranslateChildEvent_MultiPartFinalTextJoined       # multi-part final message joins parts (no truncation)
TestTranslateChildEvent_FinalTextAfterPartialsNotDuplicated
TestTranslateChildEvent_ToolCallAndResult              # EvToolCall/EvToolResult, JSON args/result, no IsOutput
TestTranslateChildEvent_ConfirmationSetsBlocked        # adk_request_confirmation → blocked, no emitted event
TestTaskHub_JoinFanIn                                  # register/finish/wait across goroutines
TestTaskHub_TaskListMonotonic                          # settled status never regresses to running
```

Add for the fixes in §6: a ctx-cancelled semaphore acquire returns a failed TaskResult (d);
hub eviction after terminal (e); chat-handler rejection of `mode: subagent` agent_id (b);
`can_dispatch` true for an agent whose tools include `task` (a); TaskFactory error does not
yield an ungoverned tool (c).

```sh
go test ./internal/tools/... ./internal/roster/... ./internal/handler/...
gofmt -l internal agents   # must print nothing for files you touched
```

### Governance checks

```sh
curl -s localhost:8080/v1/agents | jq '.data[].id'
# → contains swarm-coordinator; does NOT contain researcher/data-analyst/gap-analyst/report-writer

curl -s localhost:8080/v1/agents | jq '.data[] | select(.id=="swarm-coordinator") | .can_dispatch'
# → true (after fix (a))

# after fix (b): direct invocation of a subagent-mode agent is rejected
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"researcher","messages":[{"role":"user","content":"hi"}]}' | jq .
```

### Swarm smoke (curl)

Upstream's smoke: dispatch through swarm-coordinator and watch the parent SSE stream.

```sh
curl -N localhost:8080/v1/chat/completions -H 'content-type: application/json' -d '{
  "model": "swarm-coordinator", "stream": true,
  "messages": [{"role":"user","content":
    "Use the researcher subagent to find the capital of France, then answer."}]
}'
```

Expect on the parent stream, in order:
1. `data-agent-step` `{agent:"researcher#<8hex>", status:"started"}`
2. `data-task-list` `{id:"tasks", data.tasks:[{status:"running", agent:"researcher", title:...}]}`
3. live `data-agent-delta` parts (kind `reasoning` and/or `text`) attributed to
   `researcher#<8hex>` **while the child runs** (this is the whole point — if these only
   arrive at the end, the child is not streaming with SSE mode or the sink context is wrong)
4. `data-agent-step` `{status:"done", durationMs:>0}`
5. `data-task-list` snapshot with that row `status:"completed"`
6. the coordinator's own `text-delta` synthesis containing "Paris"

Then verify the child session is first-class:

```sh
# the session_id from the task tool result, format <parent>:researcher-<8hex>
curl -s "localhost:8080/v1/sessions/<parent>:researcher-<8hex>" | jq .
```

Parallelism: ask for two independent facts "in parallel"; confirm two `running` rows appear
in one task-list snapshot before either settles, and that the final answer contains both —
that proves background dispatch + `task_join` fan-in. Reload the thread afterwards
(GET messages / browser refresh): the projected parts must include the same
`data-agent-step`/`data-task-list` parts (projection parity).

### Browser smoke

Open the fork's UI, select the swarm coordinator, send the parallel prompt: two sub-agent
cards appear immediately with live reasoning/text, task bar shows 0/2 → 2/2 (counting
`completed`), each card links to its child session, and a page reload renders the identical
transcript. Cancel a run mid-swarm: foreground children stop (ctx derivation), and after
fix (d) a queued dispatch aborts instead of hanging.

---

*End of doc 03. Next: doc 04 (per the series index).*
