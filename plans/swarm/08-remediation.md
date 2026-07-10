# Phase 08 — Remediation & Feature Completeness

> Post-implementation review (2026-07-10) found the architecture sound but critical bugs concentrated in exactly the flows that define "opencode-like": multi-turn chat, answering questions, resuming after HITL, concurrent turns, and swarm visibility. This plan closes every finding and completes the original feature ask.

Inputs: the original requirements (docs/prompt-1.md), phase plans 00–07, and two deep code reviews (backend correctness + frontend contract). Findings are labelled C/H/M/L per the review.

---

## Part 1 — Findings vs the original ask

| Original ask | Status | Gap → workstream |
|---|---|---|
| **1. Agent swarm** — coordinator assigns typed subagents (researcher, gap-analyst, report-writer, explore, plan…); deep-research swarm possible via this pattern | ~50% — task tool + typed dispatch works (curl-verified); children **invisible** (no streaming → UI agent cards unused), **sequential only**, governance unwired (`Allowed` never set, `CanDispatch` dead), legacy JSON-board coordinators coexist | **W3** |
| **2. Task/todo list, state in Redis** | ~50% — `todowrite` + `data-task-list` render works; task state **not** in Redis; no spawn-synthesised task list | **W3** (spawn list), **W4** (Redis state) |
| **3. Internal agents** — compaction, memory, summariser/titles, suggestions, router | ~60% — all six agents exist (`/v1/route` curl-verified); **none auto-invoked**: no auto-titles, no post-run memory extraction, compaction not triggered on the new path | **W4** |
| **4. Memory** — short-term Redis, long-term OpenSearch | ~50% — services + tools pre-existed; **kNN recall injection missing**, auto-extraction missing | **W4** |
| **5. Server-side sessions** — survive disconnect, background, rejoin later, status in Redis, resumable streams (Kafka-ready) | ~45% — single-turn disconnect/replay curl-verified; **multi-turn broken (C1)**, concurrent turn dropped (C2), HITL resume bypasses log (C4), no authz (H2), status store is **in-process maps not Redis**, Redis adapter untested + non-atomic (H4), no UI resume wiring | **W1**, **W2**, **W4**, **W6** |
| **6. MCP integration** — agents access MCPs; backend-held auth (GitLab example) | ~40% — client verified end-to-end (office pptx); **static-header auth silently unwired (H3)**, `${ENV}` expansion promised-not-implemented, empty `command` panics startup, **OAuth (the actual GitLab ask) not built** | **W5** |
| **7. Question agents** — interview user, rendered in UI | ~40% — pause + opencode schema curl-verified; **answers never reach the model (C3)**, UI shows raw JSON approve/deny not a question form, resume body can't carry answers | **W2**, **W6** |
| **8. Office docs** — pptx/docx tools, execution environment | ~70% — Python MCP server verified generating real pptx; **returned download URLs are dead links** (nothing serves `/files`), docxtpl untested (no template), no object storage | **W5** |

**Cross-cutting:** identity split — REST sends `X-User-ID`, streaming/resume transports don't (streams run as `anonymous`) → **W6**. Verification gap — every critical bug lives in a flow the curl tests never exercised → **W0**.

## Full findings register

CRITICAL: C1 multi-turn broken (sticky terminal + `after=0` + pump closes at first terminal) · C2 concurrent turn silently dropped · C3 question answers are a no-op · C4 resume bypasses coordinator/log (`Coordinator.Resume`/`Cancel` dead code, no cancel route).
HIGH: H1 `after=-1` panic holds mutex → per-session deadlock DoS · H2 no authz on session attach (cross-user reads; `Start` doesn't check owner either) · H3 MCP headers unwired + empty-command startup panic · H4 Redis INCR/XADD non-atomic + `Cancel` violates single-writer (swallowed errors, lost terminals hang readers) · H5 failed runs recorded `done`; double terminal on cancel.
MEDIUM: M1 unbounded MemoryLog + coordinator maps growth · M2 task children: `context.Background()` leak, invisible (no streaming), nested `question` deadlocks child, multi-part truncation · M3 lossy replay (RunFinished double-usage, `EvHITLResolved` dropped by pump, `QuestionPayload.Questions` dead) · M4 markdown loader wholesale-replaces instead of field-merge · M5 `/` in childID unreachable via mux `{id}` · M6 `stream:false` bypasses coordinator.
LOW: MemoryLog no heartbeats · attach loses modelID · `resume.go` `registry.IDs()[0]` fragility · duplicate orchestrators · `TaskDeps.Allowed` never set.
NEW: N1 office `/files` URLs 404 (no static serving/upload). Frontend: F1 no `X-User-ID` on chat/resume transports · F2 `sessionStreamUrl`/`useSessions` unused, no reconnect/sidebar · F3 `from-history` text-only · F4 question interrupts render as generic approve/deny JSON · F5 swarm runs show only the `task` tool card.

Verified feasible: ADK `toolconfirmation.ToolConfirmation{Confirmed bool, Payload any}` reaches the tool via `ctx.ToolConfirmation()` (`tool/tool.go:68`) → answers can ride the confirmation payload; no ADK fork needed.

---

## Part 2 — Workstreams

Order: **W0 → W1 → W2 → {W3, W4, W5 in parallel} → W6** (W6 tracks contracts from W2/W3/W4/W5). Opus subagents on W1, W2, W3, W5-OAuth.

### W0 — Failing reproductions first (the review as executable spec)
Go integration tests (httptest against a wired router + fake model, mirroring `internal/handler` test patterns) + a curl smoke script:
1. Multi-turn: two sequential turns on one `thread_id` → second stream must carry turn-2 deltas (repros C1).
2. Concurrent turn while running → NOT silently dropped: queued or explicit 409 surfaced (C2).
3. Question round-trip: ask → resume with `answers` → tool result contains the answers → model sees them (C3).
4. Resume-after-HITL is event-sourced: continuation events appear in the log; `?after=` replay includes them; handle leaves `awaiting-input` (C4).
5. Cross-user attach → 404 (H2). 6. `after=-1` → 400, session still usable (H1). 7. Cancelled parent run → child ctx cancelled (M2). 8. Failed run → status `error` not `done` (H5).
**Files:** `internal/handler/sessions_integration_test.go`, `internal/agent/coordinator_test.go`, `scripts/smoke.sh`. **Exit:** all 8 fail for the documented reason.

### W1 — Run-framed event log (fixes C1, C2, H1, H5, M1; Opus subagent)
The one *design* rework: the log conflates session and run lifecycles.
- Seq stays per-session monotonic. Stamp `RunID` on `run-status` events. `Coordinator.Start` captures `StartSeq = Head()+1` and returns it in the handle.
- **`EventLog.Read` never terminal-closes** — closure policy moves to callers. Delete `memSession.terminal` (sticky-terminal bug) and `terminalEntry` early-return in Redis `Read`; subscribers persist across terminals.
- **Pump policies:** run-attach (chat POST) = attach `after=StartSeq-1`, close at first terminal (all its events are ≥ StartSeq, so first-terminal-wins is now correct). Session-follow (`/v1/sessions/{id}/stream`) = replay all, then stay live with heartbeats until client disconnect (SSE standard); emit `finish` framing per terminal without closing.
- **Concurrent turns queue** (opencode `SessionRunCoordinator` semantics): per-session pending-turn queue; run drains it on finish. Never silently drop.
- H5: `streamEvents` returns an outcome (done/error/interrupted); `finish` idempotent via `sync.Once` per run; cancel funnels its terminal append through the run goroutine (fixes half of H4).
- H1: clamp `after≥0` + `defer s.mu.Unlock()` in `MemoryLog.Read` snapshot.
- M1: evict MemoryLog sessions + coordinator `known`/`byUser` on terminal+idle TTL; cap `events` with `MAXLEN`-like trim after archive (W4).
- Move `StatusStore` to Redis hash (`runstatus:{app}:{user}:{session}` + per-user set) per the 01 plan — the original ask explicitly wanted session status in Redis; keep in-memory fallback.
**Files:** `internal/eventlog/{eventlog,memory,redis_stream}.go`, `internal/agent/{coordinator,pump,stream_coordinator}.go`, `internal/run/status.go` (new), `internal/handler/sessions.go`.

### W2 — Resume, answers, cancel (fixes C3, C4, M3-partial; Opus subagent)
- `internal/handler/resume.go` → `Coordinator.Resume` + attach-to-log; delete the synchronous `StreamResumeRunFormat` path. Continuations become logged, replayable, ownership-checked.
- `ResumeRequest` += `Answers [][]string` (+ optional `Text string`). Resume builds `FunctionResponse{Response: {"confirmed": true, "payload": {"answers": …}}}`; `questionHandler` reads `ctx.ToolConfirmation().Payload` and returns real `QuestionResult` (opencode format: `"Q"="label, label"` model-facing string). Verify the exact payload unmarshal shape in `internal/llminternal/request_confirmation_processor.go` first.
- Populate `QuestionPayload.Questions` (dead field) so the UI gets the structured schema; keep `data-tool-interrupt` for back-compat.
- Add `POST /v1/sessions/{id}/cancel` → `Coordinator.Cancel` (owner-checked).
- M3: drop the redundant `EvUsage` from `RunFinished` (carry breakdown once); give the pump an `EvHITLResolved` case (re-surface the tool call as the sync path did).
- H2: `sessions.go` 404s unless `Status(userID, id)` succeeds; `Start` rejects a session owned by another user.
**Files:** `internal/handler/resume.go`, `internal/types/chat.go`, `internal/tools/question.go`, `internal/agent/{coordinator,pump,eventlog_encoder}.go`, `internal/handler/sessions.go`.

### W3 — Swarm visibility, parallelism, governance (fixes M2, M5, LOWs; original ask #1; Opus subagent)
- **Stream children:** task tool runs the child with `StreamingModeSSE`; a child-scoped encoder translates child events into the PARENT session's log as `agent-delta`/`agent-step`/tool events with `Author=subagentType#shortID`, `SessionID=childID` → `agent-cards.tsx` lights up with zero frontend change (confirmed it keys off `data-agent-delta`). Serialise via the single run-goroutine appender from W1 (child events channel into it — no encoder concurrency).
- Child ctx derives from the tool context (cancel propagates); fix multi-part truncation (accumulate parts, don't `Reset()` per part); deny `question`/`task` to children via permissions (nested interview deadlock) until sub-coordinators are explicitly enabled.
- `childID` separator `:` not `/`; task tool-call metadata carries `childSessionId` + `subagentType` (opencode `ToolPart.metadata.sessionId` parity).
- **Parallel fan-out:** `task(background:true)` → immediate `{session_id, status:"running"}`; `task_join(session_ids)` blocks and returns `<task_result>`s; `maxParallelWorkers` semaphore. Deep-research swarm becomes genuinely parallel.
- **Spawn-synthesised `data-task-list`** merged with `todowrite` snapshots (per plan 02 §6.3).
- **Governance + cleanup:** wire `AllowedSubagents` from md frontmatter → `TaskDeps.Allowed`; derive `CanDispatch` from "tools contain `task`"; **delete** `agents/coordinator/`, `agents/swarm/`, the `task-coordinator`/`research-swarm` YAML entries and their board workers; author the curated subagent roster from the original ask as md defs (`researcher`, `data-analyst`, `gap-analyst`, `report-writer`, `explore`, `plan` — `mode: subagent`).
**Files:** `internal/tools/task.go`, `internal/agent/{coordinator,eventlog_encoder}.go`, `internal/tasks/tasks.go`, `internal/roster/*`, `config/default/agents/*.md`, deletions.

### W4 — Durability, history, memory glue (original asks #2 #3 #4 #5)
- Redis Streams: atomic Lua `INCR`+`XADD`; integration-test against a real Valkey (docker service; skip-if-unavailable); MemoryLog heartbeats.
- **Full-parts projector** `ProjectMessages` (per plan 01 §5) → persist `parts` on the OpenSearch `messages` index (+ flatten `content` for search); `session_events` archive flush on terminal; composite read (Redis hot → OpenSearch cold). `GET /v1/threads/{id}/messages` returns parts.
- Todo/task state → Redis per-session key (original ask #2).
- Memory glue: kNN recall injected into agent context pre-run (verify `memories` index has `knn_vector`); post-run hooks fire `memory_extractor` (write per-user memories) + `title` (auto thread titles) async; compaction trigger wired to the usage events from the log.
- `stream:false` (M6) and any remaining legacy path route through the coordinator.
**Files:** `internal/eventlog/{redis_stream,project,archive}.go`, `internal/chat/messages.go`, `internal/handler/threads.go`, `internal/run/hooks.go` (new), `pkg/db/opensearch/indices.go`.

### W5 — MCP auth + office polish (original asks #6 #8; Opus subagent for OAuth)
- Immediate (H3): validate `len(command)>0` at load; header injection via custom `http.Client` RoundTripper on `StreamableClientTransport`; implement the promised `${ENV}` expansion.
- **Backend-held OAuth** (the GitLab design): `POST /v1/mcp/{server}/connect` → authorize URL (PKCE, RFC 7591 dynamic registration when no clientId); `GET /v1/mcp/oauth/callback` (state/CSRF, 5-min timeout); token store encrypted at rest (reuse `ENCRYPTION_KEY`), keyed `(userID, server)`, refresh handling; RoundTripper injects the token per-user; statuses `connected|failed|needs_auth`; `GET /v1/mcp` servers+status list for the UI. Verify with a local OAuth-enabled test MCP server.
- **N1 office fix:** mount static serving for `OFFICE_OUTPUT_DIR` in `server.py` (Starlette route on the FastMCP app) — or S3 upload + signed URL for prod; add a default docxtpl template + test `render_report_docx`; smoke-test all three tools' URLs actually download.
**Files:** `internal/mcp/{manager,oauth,tokenstore,callback}.go`, `internal/config/mcp.go`, `internal/server/server.go`, `services/office-mcp/server.py`.

### W6 — Frontend (agentui; tracks W2/W3/W4/W5 contracts)
1. **F1 identity (critical, small):** `X-User-ID` on `DefaultChatTransport` (headers option in `use-agent-chat.ts`) + the resume fetch in `use-interrupt-resolver.ts` — streams and REST must share one user key.
2. **F4 question card:** dedicated renderer for question-tool interrupts (header chip, option buttons, multi-select, free-text when `custom`), posting `{thread_id, action, answers}`; show chosen answers in the settled card.
3. **F2 sessions + resume:** sidebar section from `useSessions()` (running badge); reconnect wiring — track high-water `seq` per thread, on load check `useSessionStatus` → if `running`/`awaiting-input`, attach `sessionStreamUrl(id, lastSeq)` via `readUIMessageStream` and merge (the HITL-resume merge in `use-interrupt-resolver.ts` is the template).
4. **F3 full-parts rehydration:** `from-history.ts` maps the parts array from W4 (text/reasoning/tool/data-*) instead of text-only.
5. **F5** falls out of W3 with zero changes (cards already key off `data-agent-delta`); verify multi-instance keys (`type#id`).
6. **MCP connect UI:** servers list from `GET /v1/mcp`, connect → authorize redirect, `needs_auth` badge.
**Files:** `components/chat/{use-agent-chat,use-interrupt-resolver}.ts`, new `question-card.tsx`, sidebar sessions section, `lib/chat/from-history.ts`, `lib/api/mcp.ts`.

---

## Verification (end-to-end, per workstream)

- **W0/W1:** the 8 reproductions green; curl: 3-turn conversation on one thread; kill tab mid-turn-2, rejoin from last seq → only turn-2 tail replays; concurrent POST → queued, both answers arrive in order; restart with `EVENTLOG_STORE=redis` mid-run → history intact, orphan marked `error`.
- **W2:** curl question flow → answer with options+free text → model's next output references the chosen answer; replay of the full session includes the continuation; cancel endpoint stops a run and status sticks at `cancelled`.
- **W3:** deep-research prompt → parallel children (timing overlap), live agent cards streaming in the UI, task bar reflects spawns, `<task_result>`s synthesised; legacy coordinators gone; child of a leaf cannot spawn.
- **W4:** reload mid-thread shows reasoning/tools/artifacts/cards; thread auto-titles; a fact from session A recalled in session B; valkey-backed log passes the same contract tests as memory.
- **W5:** MCP server with API-key headers works; GitLab-style OAuth connect→callback→token→tool call with browser closed; office URLs actually download; docxtpl renders a branded report.
- **W6:** all of the above visible/usable in agentui; streams and REST keyed to the same user id.
