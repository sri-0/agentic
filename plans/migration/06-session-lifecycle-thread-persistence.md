# 06 — Session Lifecycle & Thread Persistence (PRs #14–#17)

> Series: code-migration plans. Prereqs: docs 01–05 (bootstrap/config layout, coordinator +
> event log architecture, HITL resume flow, message projection/archive). Source of truth:
> merged `main` @ `17b1a87` in the reference repo. All `file:line` references below are
> **current main**, not the original PR diffs — later PRs touched some of these lines, and
> the drift is called out where it matters.

**Audience.** You are porting into a **diverged fork**. Do not blind-copy diffs. For each PR:
read the intent, find your fork's equivalent of the touched code (names/paths may differ),
and implement the behavior while preserving your fork's local changes. `git show <sha>` in
the reference repo shows the original change; the current-main snippets below show where it
ended up after subsequent PRs.

Four small PRs, one theme: a chat session must leave behind durable, listable, correctly
titled, fully rendered state — and the lifecycle knobs around it must be configurable.

| PR | SHA | One-liner |
|----|-----|-----------|
| #14 | `2f724bc` | Create the thread doc on the chat path (fixes empty sidebar + dead auto-titles) |
| #15 | `d8bc600` | Prompt fix: basic_agent calls `write_database` directly; platform owns HITL approval |
| #16 | `22279f9` | `MAX_OUTPUT_TOKENS` env override of the 8192 output cap (agent + proxy paths) |
| #17 | `64cf25a` | `SESSION_RETENTION` env, never-evict awaiting-input, ViewedStore + `/viewed` endpoint, synchronous terminal flush with `refresh=wait_for` |

---

## PR #14 — `2f724bc` · Thread doc created on the chat path

### Intent

The frontend mints a client-side `thread_id` and streams with it, but **never calls
`POST /v1/threads`**. `MessageSaver` only did `UpdateDocument` on `IndexThreads` — a no-op
when the doc is absent. Two user-visible breakages:

1. **Empty sidebar** for every user: `GET /v1/threads` lists thread docs, and none existed.
2. **Dead auto-titles**: `TitleHook.titleUnset` does `GetDocument` on the missing thread
   doc, errors, and skips title generation entirely.

Fix: the chat path upserts the thread doc on the first user message, idempotently, and the
user ID is actually threaded through the direct (non-coordinator) run paths instead of `""`.

### Key code (current main)

- `pkg/db/opensearch/client.go:127` — `CreateDocumentIfAbsent(ctx, index, docID, body) (created bool, err error)`.
  Issues `PUT /{index}/_doc/{docID}?op_type=create` via `doRaw` (`client.go:316`).
  **409 Conflict is a harmless no-op**: returns `(false, nil)`. First caller wins; later
  callers never clobber. Any other >=300 status is an error.
- `internal/chat/messages.go:31` — `SaveUserMessage(ctx, threadID, userID, content string, turn int)`.
  Inside the async goroutine (~`messages.go:47-78`):
  1. `CreateDocumentIfAbsent(IndexThreads, threadID, {user_id, title:"New Chat", created_at, updated_at})` (`messages.go:57`)
  2. If **not** created (thread already existed — a later turn): `UpdateDocument` bumps
     `updated_at` only, so the sidebar re-sorts without touching the title (~`messages.go:70`)
  3. Then indexes the user message doc as before (`messages.go:75`)
- `internal/agent/stream.go:85,90` — `StreamAgentRun` / `StreamAgentRunFormat` take
  `threadID, userID string` (was just `threadID`; callers passed `""` for the save).
- `internal/agent/nonstream.go:21` — same for `NonStreamAgentRun`.
- `internal/handler/chat.go` — the direct-path calls pass `UserID(r)` instead of `""`.
- `internal/agent/hooks.go:224,236` — `titleUnset` returns `t == "" || t == "New Chat"`,
  so the placeholder title is treated as *unset* and the TitleHook still generates a real
  title. This is the cross-package invariant behind the `"New Chat"` magic string (see
  Known issues (c)).

**Drift note:** the original `2f724bc` diff shows `SaveUserMessage(ctx, threadID, userID, content)`
(4 args, random message doc ID). A later PR (`e25c367`, deterministic message IDs) added the
`turn int` parameter — when `turn >= 0` the message doc ID is `{threadID}:{turn}:user`. Port
against whatever signature your fork has; the thread-doc upsert block is what this PR adds.

### Implementation steps

1. Add `CreateDocumentIfAbsent` to your fork's OpenSearch (or equivalent) client. The
   contract that matters: **conditional create keyed by doc ID, existing-doc conflict maps
   to `(created=false, err=nil)`**. If your fork uses a different store, use its equivalent
   (SQL `INSERT ... ON CONFLICT DO NOTHING`, Mongo `insertOne` + duplicate-key swallow, etc.).
2. In your `SaveUserMessage` (or wherever the chat path first persists a user turn), before
   saving the message: create-if-absent the thread doc with
   `{user_id, title: "New Chat", created_at: now, updated_at: now}`; on `created == false`
   bump `updated_at` only. Never write `title` on the update path.
3. Verify your title-generation guard treats the placeholder title as unset (fork's
   equivalent of `titleUnset`). If your fork uses a different placeholder, keep *its*
   placeholder consistent everywhere — see Known issues (c).
4. Thread the real user ID through every direct (non-coordinator) run entrypoint down to
   the saver. Search your fork for `SaveUserMessage(ctx, threadID, ""` — every `""` userID
   is a thread that will never list for its owner.
5. Update call sites/tests for the widened signatures (the reference PR touched 8 test
   call sites in `stream_test.go`).

### Verification

- Unit: `go build ./... && go vet ./...`; run your stream tests.
- Live: chat once with a fresh client-minted `thread_id`, then `GET /v1/threads` — the
  thread must appear with `title: "New Chat"` (or the generated title if the hook already
  ran). After the title hook fires, a **second** message must keep the generated title and
  only bump `updated_at`. Cross-user `GET /v1/threads/{id}` must still 404 (ownership).

---

## PR #15 — `d8bc600` · Prompt: call `write_database` directly, platform owns approval

### Intent

The system prompt advertised writes as `Write to the database (requires human approval)`.
Models (especially gpt-oss) read "requires human approval" as *their* job: they reasoned
themselves out of the tool call and asked "shall I proceed?" **in prose**, so the run never
suspended, no approval card rendered, and `/v1/agent/resume` was never exercised. The
`write_database` tool already sets `RequireConfirmation: true` — the **platform** owns
approval, and the model should just make the call.

### Key code (current main)

- `config/default/agents.yaml:13-16` — the capability line now reads:

  ```yaml
  - Write to the database: to insert/update/delete data, CALL the write_database
    tool directly. Do NOT ask the user for permission in prose first — the system
    automatically shows the user an approval prompt for the tool call before it
    runs, so just make the call and let the platform handle confirmation.
  ```

- The HITL mechanism itself is unchanged; it is covered by
  `TestStreamAgentRun_HITLInterrupt` (`internal/agent/stream_test.go`) and the resume path
  (`internal/handler/resume.go`).

### Implementation steps

1. Find your fork's agent config (agents.yaml or equivalent) and every agent whose prompt
   mentions approval-gated tools. Replace any "requires human approval" phrasing with an
   explicit instruction to call the tool directly and let the platform confirm.
2. Confirm the gated tools in your fork actually set the confirmation flag
   (`RequireConfirmation` or your fork's equivalent) — the prompt change is only safe
   because the platform intercepts the call.
3. If your fork's prompt has drifted (extra capabilities, different tool names), edit the
   one line — do not overwrite the whole prompt block.

### Verification

- Config-only change: restart the server, then send "insert a test row into users" (or your
  fork's equivalent write ask). Expected: the model **calls** `write_database`, the run
  suspends `awaiting-input`, an approval request is emitted on the wire (see the combined
  curl script), and no "shall I proceed?" prose appears. Deny must reject; approve must
  execute.
- Note the reference PR was landed unverified end-to-end (provider credits); PR #16 later
  unblocked and verified the full flow. Verify it in your fork.

---

## PR #16 — `22279f9` · `MAX_OUTPUT_TOKENS` override of the 8192 cap

### Intent

The outbound output-token cap was hardcoded at 8192. Providers that pre-reserve the
requested max (OpenRouter) 402 the request on low-balance accounts ("requested up to 8192
but can only afford ~1448") — blocking **every** chat before a token streams. Make the cap
env-overridable so constrained deployments keep running.

### Key code (current main)

- `pkg/genai/openai/openai.go:37` — `const DefaultMaxOutputTokens = 8192` (unchanged).
- `pkg/genai/openai/openai.go:43-50` — `var effectiveMaxOutputTokens = func() int64 {...}()`:
  reads `MAX_OUTPUT_TOKENS` from env **at package init**, falls back to the default on
  empty/invalid/non-positive. (This init-time read is Known issue (e) — fix during port.)
- `pkg/genai/openai/openai.go:53` — exported `MaxOutputTokens() int64` accessor.
- `pkg/genai/openai/openai.go:296-298` — `applyGenerationConfig` uses
  `effectiveMaxOutputTokens` as the ceiling; a smaller caller-supplied
  `cfg.MaxOutputTokens` is honored (clamp-down only).
- `internal/handler/chat.go:119` — proxy path: `capMaxTokens(body, int(openaiproxy.MaxOutputTokens()))`
  (was `DefaultMaxOutputTokens`). `capMaxTokens` itself is at `chat.go:306`.

### Implementation steps

1. Locate both places your fork sets/clamps `max_tokens`: the agent LLM call path and any
   raw proxy path. Both must share one effective cap.
2. **Implement per Known issue (e): route through your fork's config struct** (add
   `MaxOutputTokens int` with env tag `MAX_OUTPUT_TOKENS`, default 8192, in your fork's
   equivalent of `internal/config/config.go`) rather than copying the package-level
   `os.Getenv` init. Keep `DefaultMaxOutputTokens` as the constant default and keep an
   accessor if the proxy path lives in another package.
3. Preserve clamp semantics: caller-supplied smaller values win; larger values are clamped
   to the effective cap; absent values get the cap.

### Verification

- `MAX_OUTPUT_TOKENS=1024 ./server` → outbound request bodies carry `max_tokens: 1024`
  (verify via provider logs or a local echo upstream). Unset → 8192. A client sending
  `max_tokens: 256` → 256 (not raised).
- If you followed (e), also verify the value appears in your config-dump/startup log.

---

## PR #17 — `64cf25a` · Retention, viewed-state, synchronous terminal flush

Three related lifecycle changes. Read the terminate() ordering carefully — Task C's
happens-before is the whole point.

### Task A — Configurable session retention

**Intent.** Replace the hardcoded `evictIdleTTL = 30m` with a per-Coordinator retention
window from env `SESSION_RETENTION` (default 1h), clamp the sweep tick for short retentions,
and **never evict an awaiting-input (paused HITL) session** regardless of age.

**Key code (current main).**
- `internal/config/config.go:38` — `SessionRetention time.Duration \`env:"SESSION_RETENTION,default=1h"\``
- `internal/agent/coordinator.go:36` — `const defaultSessionRetention = time.Hour` (fallback).
- `internal/agent/coordinator.go:160` — `SetSessionRetention(d)`; non-positive keeps default.
- `internal/agent/coordinator.go:817,833` — `sweepLoop` uses a re-armed `time.Timer` with
  `sweepInterval() = min(5m, retention)`, recomputed each tick so a retention set after
  construction takes effect.
- `internal/agent/coordinator.go:846-864` — `sweep()`: reads `c.sessionRetention` under the
  lock; skips active sessions; **skips `h.Status == RunAwaitingInput`** (the HITL guard,
  ~`coordinator.go:858`); evicts on `UpdatedAt` older than cutoff.
- `internal/bootstrap/bootstrap.go:285-287` — wiring: `runCoordinator.SetSessionRetention(cfg.SessionRetention)`.

**Steps.** Add the config field; replace your fork's eviction-TTL const with a settable
field read at sweep time; clamp the tick; add the awaiting-input guard in the sweep loop
(this guard is a correctness fix — evicting a paused HITL session strands the approval);
wire in bootstrap. Test: `TestCoordinator_Sweep_RetentionAndHITLGuard`
(`internal/agent/coordinator_test.go:254`) — drive `sweep()` manually with a fake clock.

### Task B — Server-side per-user viewed-state

**Intent.** The "completed-but-unseen ring" UI needs a server-side flag: on a hard terminal
(done/error/cancelled — **not** awaiting-input) the session is recorded UNVIEWED for its
owner; `POST /v1/sessions/{id}/viewed` flips it; `GET /v1/sessions[/{id}]` returns `viewed`
on each handle.

**Key code (current main).**
- `internal/agent/viewed.go` (new file, whole thing) — `ViewedStore` interface
  (`SetUnviewed(ctx, sessionID, ttl)`, `MarkViewed`, `Viewed`), `MemoryViewedStore`
  fallback, `RedisViewedStore` (key `viewed:{app}:{session}`, value `"0"`/`"1"`,
  `SetUnviewed` sets TTL=retention, `MarkViewed` uses `KEEPTTL`, missing key → viewed=true
  because absence means "no recorded terminal — unseen is the exception"),
  `NewViewedStore(cfg, logger)` selecting Redis when `EVENTLOG_STORE=redis|valkey`.
- `internal/agent/coordinator.go:49-60` region — `RunHandle.Viewed bool \`json:"viewed"\``,
  filled on **copies** only (never persisted on the stored handle).
- `internal/agent/coordinator.go:155` — `SetViewedStore`.
- `internal/agent/coordinator.go:520-536` — terminate(): on hard terminal,
  `SetUnviewed(sessionID, ttl=retention)` with a 2s-bounded context; skipped for
  awaiting-input.
- `internal/agent/coordinator.go:660,675` — `Status`/`List` fill `cp.Viewed = c.viewedFlag(...)`
  **after releasing the mutex** (viewedFlag does I/O — do not hold the coordinator lock).
- `internal/agent/coordinator.go:696` — `viewedFlag`: nil store → false; non-terminal
  status → false; store lookup with 2s timeout. (Error branch is Known issue (a); per-item
  lookup loop is Known issue (b) — fix both during port.)
- `internal/agent/coordinator.go:718` — `MarkViewed(userID, sessionID) bool`: ownership
  check under lock, then store write; false → handler 404s.
- `internal/handler/sessions.go:98` — `SessionMarkViewed` handler; 404 on `!MarkViewed`.
- `internal/server/server.go:59` — route `POST /v1/sessions/{id}/viewed` (`"POST", "OPTIONS"`).
- `internal/bootstrap/bootstrap.go:288-289` — `runCoordinator.SetViewedStore(agent.NewViewedStore(cfg, logger))`.

**Steps.** Port the store (mirror your fork's existing Redis-vs-memory selection pattern —
in the reference it copies `NewTaskBoardStore`); add the handle field + fill-on-copy; add
terminate() SetUnviewed; add MarkViewed + handler + route; wire in bootstrap. Apply Known
issues (a) and (b) as you write `viewedFlag`/`List` — do not port the reference versions
verbatim. Tests: `TestCoordinator_ViewedFlow` (`coordinator_test.go:302`).

### Task C — Synchronous terminal flush, ordered before the terminal event

**Intent.** The full-parts assistant doc (tool parts included) was written by a detached
best-effort `FlushAsync` that can lag; a reload landing before it got text-only/empty
history — "tools don't render after reload". Fix: terminate() flushes the terminal turn
**synchronously** (bounded 10s) with OpenSearch `refresh=wait_for`, **ordered BEFORE
appending the terminal `EvRunStatus` event**. The terminal event is what every reader waits
on to consider the run `done`, so this ordering is a happens-before: by the time any client
can observe completion, the parts doc already exists AND is searchable. The async
ArchiveHook re-flush stays (idempotent via deterministic `_id` = `{session}:{turn}:{role}`).

**Key code (current main).**
- `pkg/db/opensearch/client.go:86` — `IndexDocumentRefresh(..., waitForRefresh bool)`:
  appends `?refresh=wait_for` to the index request. Plain `IndexDocument` (`client.go:77`)
  stays for the hot path.
- `internal/agent/archive.go:58,66,70` — `Flush` (waitRefresh=false), `FlushWaitRefresh`
  (waitRefresh=true), both delegating to unexported `flush`; the message-doc write at
  `archive.go:141` uses `IndexDocumentRefresh`.
- `internal/agent/coordinator.go:121-152` — `termFlusher TerminalFlusher` + `app` fields,
  `TerminalFlusher` interface, `TerminalFlusherFunc` adapter, `SetTerminalFlusher(f, app)`.
- `internal/agent/coordinator.go:500-518` — the terminate() flush block: runs when
  `c.termFlusher != nil && outcome.status != RunAwaitingInput`, `context.WithTimeout(10s)`,
  failure only logs ("async hook will retry"). **It sits above the
  `c.log.Append(... EvRunStatus terminal ...)` at ~`coordinator.go:540`. Preserve that
  order in your fork — it is the entire fix.**
- `internal/bootstrap/bootstrap.go:290-295` — wiring:
  `runCoordinator.SetTerminalFlusher(agent.TerminalFlusherFunc(archiver.FlushWaitRefresh), cfg.AppName)`.

**Steps.** Add the refresh-capable index call to your store client (`refresh=wait_for` is
OpenSearch/Elasticsearch; for other stores use whatever makes the write immediately
readable — often nothing needed for strongly consistent stores, in which case a plain
synchronous flush suffices). Split your archiver's flush into hot/terminal variants. Add
the flusher seam + terminate() block *above* the terminal-event append. Wire in bootstrap.
Test: `TestCoordinator_TerminalFlush_BeforeTerminalEvent` (`coordinator_test.go:339`) —
the flusher callback asserts no terminal event is in the log yet at flush time. Port this
test; it locks the ordering against future refactors.

### Verification (PR #17 as a whole)

- `go test ./internal/agent/ -run 'TestCoordinator_(Sweep_RetentionAndHITLGuard|ViewedFlow|TerminalFlush_BeforeTerminalEvent)'`
- Live, on isolated ports: `SESSION_RETENTION=10s` → a done session drops from
  `GET /v1/sessions` within ~13s (retention + clamped tick), while an awaiting-input
  session survives indefinitely. Default (unset) → 1h. Viewed round-trip and flush race:
  see the combined curl script.
- Flush race check: immediately after a tool-using run reports done, `GET
  /v1/threads/{id}/messages` must include the tool parts on the assistant message —
  repeat several times (the reference verified 4/4; pre-fix was ~1/3 missing).

---

## Known issues — fix during port

These shipped on main. Implement the **corrected** behavior in the fork; do not copy them.

**(a) `viewedFlag` error fail-safe is INVERTED** — `internal/agent/coordinator.go:696-712`.
On a store error it returns `false` (unviewed), so **every Redis blip renders the unread
ring on every finished session** — a false "you have unseen results" signal. The store's own
convention (`viewed.go`: nil/missing key → `true`) and the interface comment agree that
viewed is the safe default. Implement **error → `true`** (and log the error). The only
false-return paths should be: nil store is fine as `false` (feature off), non-terminal
status → `false`.

**(b) `List()` does one sequential viewed lookup per session** — `coordinator.go:675-689`.
Each handle copy gets its own `viewedFlag` call with a **fresh 2s timeout**: N sessions ×
Redis RTT sequentially, and a degraded Redis turns `GET /v1/sessions` into an
N×2s stall. Fix in the fork: batch the lookup — add a `ViewedMany(ctx, sessionIDs)
(map[string]bool, error)` to the store (Redis `MGET` on the `viewed:{app}:{sid}` keys;
memory store reads its map once) — or at minimum create **one** shared 2s context for the
whole List call. Keep `Status()` on the single lookup.

**(c) `"New Chat"` magic string in 3 files** — `internal/chat/messages.go:59` (written on
create), `internal/agent/hooks.go:125,236` (titleUnset treats it as unset),
`internal/handler/threads.go:82` (default on explicit thread create). This literal holds a
cross-package invariant: if any copy drifts, either auto-titles stop firing or generated
titles get clobbered. Extract a shared exported const in the fork (e.g.
`chat.DefaultThreadTitle` or a small shared package if import cycles bite —
`chat` ← `agent` ← `handler` all need it) and reference it from all three sites.

**(d) `UpdateDocument` bump runs unchecked after a failed create** — `messages.go:63-73`.
When `CreateDocumentIfAbsent` returns an error, `created` is `false`, and the code falls
into the `!created` branch anyway — issuing an `updated_at` bump against a thread doc that
may not exist (and whose create just failed, e.g. OpenSearch down). On some stores a
partial-update-on-missing errors noisily; on others it can spawn a malformed doc. Fix:
`if err != nil { log; } else if !created { bump }` — skip the bump on create error. Also
check the `UpdateDocument` return value and log it (it is currently discarded).

**(e) `MAX_OUTPUT_TOKENS` read at package init** — `pkg/genai/openai/openai.go:43`.
A package-level `var ... = func() {...}()` reading `os.Getenv` at init is inconsistent with
every other knob (all flow through `internal/config` env tags), untestable without process
restarts, and silently ignores values set after init. Route it through `Config` (see PR #16
steps): add `MaxOutputTokens` to the config struct and pass it (or set it on the model/proxy
at bootstrap). Keep an accessor for cross-package reads if needed.

**(f) Global OPTIONS CORS handler is load-bearing** — `internal/server/server.go:34`:

```go
r.PathPrefix("/").Methods(http.MethodOptions).HandlerFunc(func(http.ResponseWriter, *http.Request) {})
```

This arrived on #17's branch and is easy to drop as "unrelated" during a port. It is not.
gorilla/mux `Router.Use` middleware only runs when a route **matches**, and routes
registered `Methods("GET")` only match GET — so a browser preflight (`OPTIONS`) to a
GET-only route (e.g. `/v1/models`, `/v1/sessions`; the `X-User-ID` header makes even GETs
non-simple) matches nothing and returns **405**, which the browser surfaces as a CORS
failure. The catch-all OPTIONS route makes every preflight match (the CORS middleware then
writes the headers). The fork MUST include this (or `mux.CORSMethodMiddleware` +
per-route OPTIONS everywhere — the catch-all is simpler). If your fork is not on
gorilla/mux, verify its router runs CORS middleware on unmatched-method preflights; if it
does, skip this.

---

## Combined fork-adaptation notes

- **Port order**: #14 → #16 → #15 → #17. #14 is standalone. #16 before #15 if your
  provider account is balance-constrained (#15's verification needs working chats). #17
  builds on the coordinator/archiver from docs 03–05.
- **Signature drift**: current main's `SaveUserMessage` takes `turn int` and
  `StreamAgentRunFormat` takes `userID` — if your fork already diverged on these
  signatures, keep your shapes; the port is (1) the thread-doc upsert block, (2) real
  userID reaching the saver, nothing else.
- **Store abstraction**: everything here assumes OpenSearch (threads/messages) + Valkey
  (viewed/task boards). Map to your fork's stores by contract: conditional-create (#14),
  read-your-writes on terminal flush (#17-C), TTL'd per-key flag (#17-B).
- **Do not regress the ordering invariants**: (i) terminal flush BEFORE terminal event
  append (#17-C); (ii) viewed `SetUnviewed` on hard terminal only, never awaiting-input;
  (iii) sweep never evicts active or awaiting-input. Port the three coordinator tests —
  they encode all of these.
- **Apply Known issues (a)–(f) as you write the code**, not as a follow-up pass. (a), (b),
  (d) are behavior fixes inside code you are porting anyway; (c), (e) are trivial while the
  files are open; (f) is a checklist item on the router.
- Ownership checks everywhere: threads GET (user_id scoping), `/viewed` (404 on
  non-owner), resume (H2). Confirm your fork's `UserID(r)` equivalent feeds all three.

## Curl verification script

```bash
#!/usr/bin/env bash
# Verify PRs #14-#17 end-to-end. Adjust BASE/USER/MODEL to the fork.
set -euo pipefail
BASE=${BASE:-http://localhost:8080}
U='-H "X-User-ID: verify-user"'   # or your fork's auth header
H=(-H "Content-Type: application/json" -H "X-User-ID: verify-user")
TID="verify-$(date +%s)"          # client-minted thread id, like the frontend

echo "== #14: thread doc appears + title auto-generates =="
curl -sN "${H[@]}" -X POST "$BASE/v1/chat/completions?format=aisdk" \
  -d '{"agent_id":"test-agent","thread_id":"'"$TID"'","stream":true,
       "messages":[{"role":"user","content":"Tell me one fact about Tokyo"}]}' >/dev/null
curl -s "${H[@]}" "$BASE/v1/threads" | tee /tmp/threads.json | grep -q "$TID" \
  && echo "PASS thread listed" || echo "FAIL thread missing from sidebar list"
sleep 8   # let TitleHook run
TITLE=$(curl -s "${H[@]}" "$BASE/v1/threads/$TID" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("title",""))')
[ -n "$TITLE" ] && [ "$TITLE" != "New Chat" ] \
  && echo "PASS title auto-generated: $TITLE" || echo "FAIL title still unset: '$TITLE'"
# 2nd message must NOT clobber the title (only bump updated_at)
curl -sN "${H[@]}" -X POST "$BASE/v1/chat/completions?format=aisdk" \
  -d '{"agent_id":"test-agent","thread_id":"'"$TID"'","stream":true,
       "messages":[{"role":"user","content":"and one more"}]}' >/dev/null
T2=$(curl -s "${H[@]}" "$BASE/v1/threads/$TID" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("title",""))')
[ "$T2" = "$TITLE" ] && echo "PASS title preserved on turn 2" || echo "FAIL title clobbered: '$T2'"

echo "== #16: MAX_OUTPUT_TOKENS respected (restart server with MAX_OUTPUT_TOKENS=1024 first) =="
# Observe outbound max_tokens via provider/proxy logs; chat must succeed, not 402.

echo "== #17-B: viewed round-trip =="
SID=$TID   # session id == thread id on the coordinated path
curl -s "${H[@]}" "$BASE/v1/sessions/$SID" | grep -q '"viewed":false' \
  && echo "PASS finished session starts unviewed" || echo "FAIL not unviewed after terminal"
curl -s "${H[@]}" -X POST "$BASE/v1/sessions/$SID/viewed" | grep -q '"viewed":true' \
  && echo "PASS mark-viewed 200" || echo "FAIL mark-viewed"
curl -s "${H[@]}" "$BASE/v1/sessions/$SID" | grep -q '"viewed":true' \
  && echo "PASS viewed persisted" || echo "FAIL viewed did not stick"
curl -s -o /dev/null -w '%{http_code}' -H "X-User-ID: intruder" \
  -X POST "$BASE/v1/sessions/$SID/viewed" | grep -q 404 \
  && echo "PASS cross-user viewed -> 404" || echo "FAIL ownership hole on /viewed"

echo "== #17-C: tool parts present immediately after done (run 4x) =="
for i in 1 2 3 4; do
  T="flush-$i-$(date +%s)"
  curl -sN "${H[@]}" -X POST "$BASE/v1/chat/completions?format=aisdk" \
    -d '{"agent_id":"test-agent","thread_id":"'"$T"'","stream":true,
         "messages":[{"role":"user","content":"use the calculate tool: 17*23"}]}' >/dev/null
  curl -s "${H[@]}" "$BASE/v1/threads/$T/messages" | grep -q 'calculate' \
    && echo "PASS $i: tool part in history immediately" || echo "FAIL $i: tool part missing (flush race)"
done

echo "== #17-A: retention (restart with SESSION_RETENTION=10s on an isolated port) =="
# done session must vanish from GET /v1/sessions within ~15s; an awaiting-input one must not.

echo "== #15 + HITL wire sequence: approve and deny =="
# Stream a write ask; expect on the aisdk wire (in order):
#   data: {"type":"tool-approval-request","approvalId":...}   (encoder.go:316-343)
#   run-status -> awaiting-input                              (session handle status)
TIDW="hitl-$(date +%s)"
curl -sN "${H[@]}" -X POST "$BASE/v1/chat/completions?format=aisdk" \
  -d '{"agent_id":"test-agent","thread_id":"'"$TIDW"'","stream":true,
       "messages":[{"role":"user","content":"Insert user bob@example.com into the users table"}]}' \
  | tee /tmp/hitl.sse | grep -q 'tool-approval-request' \
  && echo "PASS approval requested on wire (no prose ask)" || echo "FAIL model asked in prose / no suspend"
curl -s "${H[@]}" "$BASE/v1/sessions/$TIDW" | grep -q 'awaiting-input' \
  && echo "PASS session awaiting-input" || echo "FAIL session not suspended"
# Approve: run resumes, tool executes, terminal done.
curl -sN "${H[@]}" -X POST "$BASE/v1/agent/resume" \
  -d '{"thread_id":"'"$TIDW"'","action":"approved"}' | grep -q 'done' \
  && echo "PASS approve resumed to done" || echo "FAIL approve path"
# Deny (fresh thread): tool must NOT execute; run terminates cleanly.
TIDD="hitl-deny-$(date +%s)"
curl -sN "${H[@]}" -X POST "$BASE/v1/chat/completions?format=aisdk" \
  -d '{"agent_id":"test-agent","thread_id":"'"$TIDD"'","stream":true,
       "messages":[{"role":"user","content":"Delete all rows from users"}]}' >/dev/null
curl -sN "${H[@]}" -X POST "$BASE/v1/agent/resume" \
  -d '{"thread_id":"'"$TIDD"'","action":"denied"}' >/dev/null \
  && echo "PASS deny accepted (verify no write landed in DB)" || echo "FAIL deny path"
```

Expected HITL sequence on the wire: `tool-approval-request` → session `awaiting-input`
(and sweeper-immune) → `POST /v1/agent/resume {action:"approved"|"denied"}` → resumed
stream → terminal flush → terminal `run-status: done`.
