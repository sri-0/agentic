# 07 — Message Identity & the Session-Aware Read Path

> **Series:** agentic → fork migration, doc 07. Prerequisites: docs 01–06 (event log +
> coordinator, archiver, AI-SDK encoder, HITL/questions, sessions/reconnect, hooks).
> **Source repo:** `/Users/sri/code/agentic` at merged main `17b1a87`.
> **Covers:** PR #20 `e25c367`, PR #21 `67bc423`, PR #22 `895c03c`, PR #23 `fa185fd`.

## ⚠ Read this first

This is the **subtlest backend phase in the whole series**. Nothing here is a big diff —
the four PRs total ~450 changed lines — but every line participates in a single
cross-cutting invariant (one deterministic identity per message, produced identically by
three independent code paths). Get one producer subtly wrong and you re-introduce the
exact bugs this phase was built to kill: duplicate-key errors on reconnect, blank threads
on return-mid-stream, questions re-raised after they were answered, footers/timers wiped
on reload.

Do **not** blind-copy the diffs. Your fork has diverged — different run-lifecycle plumbing,
possibly different handler signatures, possibly its own message-id scheme already. Map
your fork's equivalents FIRST (see "Fork adaptation" below), then implement the invariant
in your fork's terms. Also read "Known issues — fix during port": upstream shipped this
phase with real defects (an O(full-history) read under a global mutex, a turn-0 clobber
path, and zero tests). The port should fix them, not inherit them.

---

## 1. Purpose

Before this phase, three different components each minted their **own** id for the same
logical assistant message:

- the live AI-SDK stream let the client generate a message id (bare `{type:"start"}` frame);
- the reconnect replay generated another;
- the DB archive keyed rows by yet another (or a random UUID for user messages).

Result: the client's keyed upsert could never recognise "this is the same message I
already have" — reconnects duplicated messages (dup-key errors) and a thread reopened
mid-stream rendered blank until the run finished.

The fix is to mint the identity **once, at the event source**, using the archiver's
existing deterministic scheme:

```
{sessionID}:{turn}:{role}         e.g.  th_abc123:0:user
                                        th_abc123:0:assistant
                                        th_abc123:1:assistant
```

where `turn` is the 0-based index of the assistant turn, derived by folding the session
event log through the **same projector** the archiver uses. On top of that identity, this
phase makes the thread-messages read path *session-aware* (fold the in-progress turn from
Redis and merge it over archived rows — a live thread loads exactly like a settled one),
persists per-message metadata (model / agent / duration, real reasoning window), and adds
the `agent_id:"auto"` classifier route.

---

## 2. The identity invariant (memorise this)

> **Live encoder output, replay/reload projection, and the DB archive must produce
> IDENTICAL message ids and IDENTICAL part shapes for the same turn.**

Three producers, one projector, one id scheme:

```
                        ┌──────────────────────────────────────────┐
                        │        Session event log (source)        │
                        │   Redis hot window + OpenSearch cold     │
                        │        (CompositeLog, seq-ordered)       │
                        └──────┬──────────────┬──────────────┬─────┘
                               │              │              │
              ①  LIVE STREAM   │   ② SESSION-AWARE RELOAD    │   ③ ARCHIVE (DB)
                               │              │              │
        coordinator startLocked│   handler projectLive-      │  Archiver.flush
        → nextTurnLocked       │   Messages →                │  → eventlog.
        → eventlog.NextTurn ───┤   eventlog.ProjectMessages ─┤  ProjectMessages
                               │              │              │
        aisdk encoder start    │   ThreadMessage rows        │  OpenSearch _id
        frame messageId =      │   ID =                      │  =
        {sess}:{turn}:assistant│   {sess}:{turn}:{role}      │  {sess}:{turn}:{role}
                               │                             │
        user msg SaveUser-     │   parts = projector Part[]  │  parts = projector
        Message doc id =       │   (same Go struct)          │  Part[] (same struct,
        {sess}:{turn}:user     │                             │  json.Marshal'd)
```

Why it holds by construction in upstream:

- **One turn oracle.** `eventlog.NextTurn(events)` (`internal/eventlog/project.go:117`)
  folds the log through the *same* `projector` that `ProjectMessages`
  (`project.go:100`) uses — minus the trailing `flush()`. The projector only advances
  `p.turn` when an assistant message actually materialises (`flush()` around
  `project.go:359+`), so:
  - a log whose last run is still open (mid-run or awaiting-input) reports the OPEN
    message's turn → a HITL resume continues under the **same** id;
  - re-projections of a growing log are stable — the Nth assistant turn is always `N-1`.
- **One part shape.** The archiver, the live-fold read path, and (indirectly, via the
  event pump) the live encoder all express parts as `eventlog.Part`. Whenever the live
  encoder learns a new frame (interrupt cards, reasoning timing, metadata), the projector
  MUST learn the equivalent fold in the same PR — PR #21 (EvQuestion/EvHITLResolved) and
  PR #22 (EvMetadata, reasoning Started/EndedMs) are exactly that.
- **One id format string.** `%s:%d:%s` / `%s:%d:assistant` appears in four places
  (encoder.go:47, chat/messages.go:~36, archive.go:~141, threads.go:~283 and ~374). In
  the fork, extract a single helper (e.g. `eventlog.MessageID(session, turn, role)`) and
  call it everywhere — upstream did not, and that is a standing copy-drift risk.

**Golden test (strongly recommended, upstream shipped none):** build a synthetic event
sequence covering text, reasoning (multiple deltas), tool call/result, question +
resolved, metadata, and a terminal. Feed it to (a) `ProjectMessages` and (b) a recorded
live-encoder frame transcript folded by your client-side reducer (or at minimum the
encoder's emitted `messageId`s and part discriminators). Assert ids and part shapes are
byte-identical. This single test makes the dup-key / blank-on-return class of bug
structurally impossible to reintroduce.

---

## 3. PR #20 — deterministic message id + async title at run start (`e25c367`)

### Intent

Mint `{session}:{turn}:{role}` once at the event source; stamp it on the AI-SDK `start`
frame AND on the persisted user-message doc. Add a run-START hook seam and use it to
generate the thread title asynchronously the moment the run begins (post-run `TitleHook`
stays as an idempotent fallback). Expose `start_seq`/`turn` on session status so clients
can attach precisely.

### Key code (upstream, at 17b1a87)

| What | Where |
|---|---|
| `NextTurn(events) int` | `internal/eventlog/project.go:109-122` |
| `RunHandle.Turn` field (json `turn`) | `internal/agent/coordinator.go:54` |
| `nextTurnLocked` (reads log, folds) | `internal/agent/coordinator.go:394-411` |
| Turn stamped: `startLocked` | `coordinator.go:346-347, 355` |
| Turn stamped: queued estimate | `coordinator.go:327` (`h.Turn + len(pending)`) |
| Turn stamped: `Resume` (HITL) | `coordinator.go:605-606` |
| `RunRequest.turn` → `SaveUserMessage(..., turn)` | `coordinator.go:456` |
| `SaveUserMessage` doc id (`-1` → random uuid) | `internal/chat/messages.go:31-40` |
| Encoder carries session+turn | `internal/stream/aisdk/encoder.go:26-49` (`New(sink, model, agentID, sessionID, turn)`) |
| `RunStarted` emits `{type:"start", messageId}` | `encoder.go:98-103` |
| Encoder built AFTER `core.Start` | `internal/agent/stream_coordinator.go` `StreamAgentRunBackground` (~line 36-50) |
| Attach/replay threads the turn | `StreamSessionAttach(..., turn, ...)` in `stream_coordinator.go`; callers `internal/handler/sessions.go:68`, `internal/handler/resume.go:111` |
| Run-start hook seam | `coordinator.go:211-221` (`AddRunStartHook`), `fireHooks` `:234-247`, fired in `startLocked` `:376-385` |
| `TitleStartHook` | `internal/agent/hooks.go:107-136`; shared `generateAndSetTitle` `:138+` |
| Bootstrap wiring | `internal/bootstrap/bootstrap.go:~418-422` |
| Legacy paths pass `-1` | `internal/agent/stream.go:117`, `nonstream.go:39`, `handler/chat.go:215` (proxy encoder gets `"", -1`) |

### Porting steps

1. **Projector first.** Add `NextTurn` to your fork's projector package. It must reuse
   the projector type verbatim (`p := &projector{...}; for … p.fold(ev); return p.turn`)
   — do NOT reimplement turn counting independently, that's how the three producers drift.
2. **Coordinator.** Add `Turn int \`json:"turn"\`` to your run-handle. Compute it at
   run start and at HITL resume (see Known issue (a) for HOW to compute it — do not copy
   upstream's under-lock synchronous read). Stamp it into the run request so the
   user-message save can use it.
3. **User message id.** Extend your `SaveUserMessage` (or equivalent) with a `turn int`
   param: `turn >= 0` → deterministic `{thread}:{turn}:user` doc id; negative → the old
   random id. Keep the `-1` sentinel on every legacy/non-coordinated call site. Fix the
   error path per Known issue (b).
4. **Encoder.** Extend your AI-SDK encoder constructor with `sessionID string, turn int`;
   empty/negative ⇒ no `messageId` on the start frame (client generates one, old
   behaviour — required for the plain-model proxy path). All other frames are
   byte-identical to before.
5. **Order of operations.** In the coordinated streaming handler the encoder must be
   constructed AFTER the coordinator `Start` returns, because the handle carries the
   turn. If your fork builds the encoder eagerly (upstream used to), restructure: build
   the sink first, encoder after `Start`; on `Start` failure fall back to
   `newEncoder(..., -1)` for the error frames.
6. **Replay parity.** Every attach/replay entry point (reconnect stream, HITL resume
   stream) must pass the handle's `Turn` into the encoder so the replayed start frame
   carries the SAME id as the original live stream.
7. **Run-start hooks.** Add `AddRunStartHook` + a shared panic-guarded `fireHooks`;
   fire start hooks from `startLocked` with the turn's input messages,
   `Status: RunRunning`. They must be fully async and never observe the run outcome.
8. **TitleStartHook.** Port with its two subtleties intact: (i) a *not-found* thread doc
   counts as "unset" (the doc is upserted async by SaveUserMessage and may not be indexed
   yet — post-run `TitleHook.titleUnset` treats not-found as "don't touch", which is
   correct there, wrong here); (ii) `generateAndSetTitle` re-checks `titleUnset` right
   before the write so user renames and the start/terminal hooks racing never clobber.
   Keep post-run `TitleHook` registered as the fallback.
9. **Session status.** Upstream's status endpoint serialises the handle directly
   (`handler/sessions.go:35` `writeJSON(w, h)`), so `start_seq` and `turn` ride along via
   json tags. If your fork builds a DTO instead, add both fields explicitly.

### Verification

- `POST /v1/chat/completions` (coordinated, AI-SDK format): first SSE frame is
  `{"type":"start","messageId":"<thread>:0:assistant"}`.
- OpenSearch (or your store): user doc `_id == <thread>:0:user`, assistant doc
  `_id == <thread>:0:assistant`. Second turn → `:1:`.
- Reconnect (`GET /v1/sessions/{id}/stream?after=…`) replays a start frame with the
  **identical** messageId. HITL approve/deny resume: continuation streams under the same
  turn's id (open message not yet flushed).
- Thread title appears within a few seconds of run START while the response is still
  streaming; renaming a thread then running again does not overwrite the rename.
- `GET /v1/sessions/{id}` shows `start_seq` and `turn`.

---

## 4. PR #21 — session-aware `GET /v1/threads/{id}/messages` (`67bc423`)

### Intent

Open a live thread the SAME way as a historical one: one fetch of fully-folded messages,
then a tail-only live stream — instead of the client replaying thousands of raw deltas
(which froze the UI and looked blank). Also teach the projector the HITL question shapes
so an awaiting-input reload shows the question card, and an answered one never re-raises it.

### Key code

| What | Where |
|---|---|
| Session-aware handler | `internal/handler/threads.go:234-323` (`ThreadsMessagesList(osClient, coord, logger)`) |
| Active-run gate | `threads.go:327-329` (`isActiveRun`: running / awaiting-input / queued) |
| Terminal-transition window fold | `threads.go:306-315` + `hasMessageID:333` |
| `projectLiveMessages` (full-log fold → ThreadMessage rows) | `threads.go:342-387` |
| `mergeLiveMessages` (upsert by id, stable sort by created_at) | `threads.go:389-413` |
| `EvQuestion` fold → dynamic-tool + `data-tool-interrupt` parts | `internal/eventlog/project.go:269-302` |
| `EvHITLResolved` fold → `resolved: approved\|denied` on the interrupt part | `project.go:304-330` |
| `KindApproved`/`KindDenied` consts | `internal/eventlog/event.go:42-50` |
| Kind stamped on the resolve event | `internal/agent/coordinator.go` `Resume` (~`:621-627`) |
| `PartToolInterrupt = "data-tool-interrupt"` | `project.go:75` |
| Route wiring gains `coord` | `internal/server/server.go:109` |

### Response envelope (client contract — document it in the fork)

- **Settled thread:** plain JSON array of `ThreadMessage` (unchanged wire shape).
- **Active run:** `{"data":[...], "live":{"head_seq":N, "turn":T, "status":"running"}}`.
  `head_seq` is the last event-log seq folded into the payload — the client attaches the
  live stream at exactly `?after=head_seq` (tail only, no gap, no overlap).
- **Terminal-transition window** (run just finished, async archive flush not yet
  searchable — detected by `hasMessageID(messages, "{thread}:{turn}:assistant")` being
  false): the log is folded and merged, but the response stays the plain settled array
  (nothing live to attach to). Idempotent: once the archive lands, the deterministic id
  yields identical content.

See Known issue (d) — consider an always-envelope shape in the fork.

### Porting steps

1. **Projector folds first** (they serve the archive too): add `EvQuestion` →
   *two* parts, exactly mirroring what your live pump emits on an interrupt: a
   `dynamic-tool` part (`state:"input-available"`, input = `Details["details"]`) plus a
   `data-tool-interrupt` part (`toolCallId`, `toolName`, `prompt`, `details`,
   `threadId`). Track `interruptIdx[toolCallID]`. Then `EvHITLResolved` → set
   `data["resolved"] = "approved"|"denied"` on the tracked interrupt part and
   re-surface/refresh the originating tool call. Missing `Kind` (old events) defaults to
   approved. Compare against your fork's live `ToolInterrupt` encoder frame field-by-field
   — the part shapes must match or an awaiting-input reload renders differently from live.
2. **Ownership gate.** The live fold must only run when `coord.Status(userID, threadID)`
   confirms the requesting user owns the session — that is the leak guard; do not gate on
   thread id alone.
3. **`projectLiveMessages`.** Read the FULL log (`Read(ctx, threadID, 0, false)`) through
   your composite hot/cold log, skip heartbeats (seq < 0), track max seq as `head`, fold
   with `ProjectMessages`, and emit rows with `ID = {thread}:{turn}:{role}` and
   `created_at` derived from the turn's first event ts (mirrors the archiver — the merge
   sort depends on this).
4. **`mergeLiveMessages`.** Upsert by id (projection wins — it is at least as fresh as
   the archive), but keep the archived row's `CreatedAt` (ordering stability) and `Model`
   when the projection lacks them; append unknown ids; `sort.SliceStable` by `CreatedAt`
   so archived user rows keep their position ahead of a same-second assistant turn.
5. **Degrade gracefully:** store-read failure no longer early-returns an empty array —
   fall through so a live session can still be folded; fold failure logs a warning and
   returns archived rows only.

### Verification

- Mid-run `GET /v1/threads/{id}/messages` returns `{data, live}`; the in-progress
  assistant turn is present, fully folded (upstream verified 1709 parts mid-swarm), and
  attaching at `?after=<head_seq>` continues mid-sentence with zero duplicated deltas.
- Reload while a question is pending shows the question card; reload after answering
  shows it resolved (approved/denied badge) and does NOT re-raise the modal.
- Reload issued within ~1s of run end shows the full final turn (terminal-window fold).
- Settled thread: byte-identical plain-array response to before the port.

---

## 5. PR #22 — metadata + reasoning duration persistence (`895c03c`)

### Intent

The footer (model · agent · duration) and the "Thought for Ns" label were blank after
reload because the projector dropped `EvMetadata` and reasoning timing. Fix ONCE in
`ProjectMessages` so it covers BOTH the archive and the PR #21 live-fold path — this is
the invariant paying off: one fold fix, two read paths healed.

### Key code

| What | Where |
|---|---|
| `ProjectedMessage.Model/AgentID/DurationMs` | `internal/eventlog/project.go:14-24` |
| `Part.StartedMs/EndedMs` (reasoning window) | `project.go:45-53` |
| Reasoning fold captures first/last delta Ts | `project.go:183-195` |
| `case EvMetadata` fold | `project.go:332-345` |
| `flush()` carries metadata; resets it | `project.go:~359-380` |
| Archive doc gains `model`/`agent_id`/`duration_ms` | `internal/agent/archive.go:126-137` |
| `messageFromHit` reads them (+ `getInt64` helper) | `internal/handler/threads.go:611-660` |
| `projectLiveMessages` carries them | `threads.go:371-385` |
| `ThreadMessage.AgentID/DurationMs` API fields | `internal/types/threads.go:30-31` |

### Porting steps

1. Add the three metadata fields to your projected-message type and the two timing fields
   to your reasoning `Part`; fold `EvMetadata` (model/author/duration — your fork's event
   names may differ) onto the OPEN message; capture the first reasoning delta's `Ts` as
   `StartedMs` (only if unset — a resumed reasoning block must not reset it) and every
   delta's `Ts` as `EndedMs`.
2. Reset all of it in both `ensureOpen` and `flush` — leaking turn-1 metadata onto turn 2
   is the classic bug here.
3. Thread the fields through all THREE consumers: archive doc (omit when zero-valued),
   `messageFromHit` (numbers arrive as `float64` from JSON — port `getInt64`), and the
   live-fold rows. Add `agent_id`/`duration_ms` to the API message type.
4. Check your fork's part JSON casing: upstream uses `startedMs`/`endedMs` (opencode
   style) — the frontend rehydration expects exactly that; match whatever your fork's UI
   reads or fix both sides together.

### Verification

- Finished turn, then reload: message JSON carries `model`, `agent_id`, `duration_ms`,
  and the reasoning part carries `startedMs`/`endedMs` with a plausible real window
  (upstream measured 942 ms). "Thought for Ns" survives reload with the SAME N as live.
- Values identical whether the row came from the archive (settled) or the live fold
  (mid-run reload) — spot-check both.

---

## 6. PR #23 — Auto agent via classifier in chat (`fa185fd`)

### Intent

`agent_id:"auto"` resolves server-side to a concrete agent through the existing `/v1/route`
classifier, in the same request — no extra client round-trip. The picked agent then flows
through the untouched pipeline (registry.Get, override-core, streaming), and the existing
`message-metadata.agentId` already shows the UI which agent was picked.

### Key code

| What | Where |
|---|---|
| `ClassifyAgent(ctx, cfg, internalAgents, sessionSvc, userID, msg, logger)` | `internal/handler/route.go:47-89` (factored out of the `/v1/route` handler at `:27`) |
| Fallback chain | roster from `cfg.Agents` minus `Internal`; run internal `"router"` agent; `normaliseAgentID` (`route.go:91+`, exact-then-substring match); fallback = first non-internal agent |
| `"auto"` branch in Chat | `internal/handler/chat.go:69-78` |
| Chat signature gains `internalAgents *agents.Registry, sessionSvc session.Service` | `chat.go:29`; wiring `internal/server/server.go:46` |

### Porting steps

1. Factor your fork's classification (however `/v1/route` does it) into a context-taking
   helper; keep `/v1/route` a thin wrapper so both callers share one behaviour.
2. In the chat handler, resolve `explicitAgentID == "auto"` through the helper BEFORE any
   registry lookup, using the LAST user message content as the classification input; log
   the routed id. Nothing downstream changes.
3. Thread the two new dependencies through your router wiring and fix test call sites
   (upstream touched 4 test files for the signature change alone — expect the same).
4. Address Known issue (e): name the sentinel, and decide `model:"auto"` behaviour.

### Verification

- `agent_id:"auto"` + "Research the French Revolution in depth" → routed to
  `deep-research` (check the log line `chat: auto-routed to agent` and the
  `message-metadata` frame's `agentId`).
- `agent_id:"auto"` + "hi" → routed to the basic/general agent. Two distinct ids proves
  prompt-based routing, not fallback.
- Classifier unavailable (stop the router agent / nil registry) → falls back to the first
  non-internal agent, request still succeeds.

---

## 7. Known issues — FIX DURING PORT (do not copy verbatim)

These shipped in upstream. The fork should land the corrected versions.

**(a) `nextTurnLocked` reads the FULL event log synchronously under the coordinator's
single global mutex, with `context.Background()`.**
`coordinator.go:400-411`: `c.log.Read(context.Background(), sessionID, 0, false)` — via
the CompositeLog that is Redis hot window PLUS cold OpenSearch replay — is O(full
history) and unbounded, while `c.mu` is held. One slow/hung OpenSearch call while
starting a turn on a 13k-event session stalls **every** session on the coordinator.
Fix options (either is fine): (i) compute the turn OUTSIDE the lock with a bounded
context (e.g. 2–5 s) before taking `mu`, re-validating head hasn't moved; or (ii) cache
`nextTurn` on the session handle and advance it at terminal flush (the projector already
tells you whether an assistant message materialised), falling back to a full fold only on
cold start of an unknown session.

**(b) `nextTurnLocked`'s error path returns turn 0 → data loss.**
On a read error it `return 0`, and `defaultRunFunc` then calls
`SaveUserMessage(..., 0)` which UPSERTS `{session}:0:user` — silently overwriting the
thread's FIRST user message with the latest one on any transient Redis/OpenSearch blip.
The `-1` sentinel → random-id fallback already exists in `SaveUserMessage`
(`chat/messages.go:31-40`); use it: on error return `-1` (and skip stamping the start-frame
messageId for that turn — a client-generated id for one turn is strictly better than
clobbering history). Log loudly.

**(c) Queued-handle turn estimate is wrong after assistant-less runs.**
`coordinator.go:327`: a queued handle gets `Turn: h.Turn + len(c.pending[...])`. If a run
terminates WITHOUT materialising an assistant message (error before first token,
cancelled instantly), the projector does not advance the turn, so the estimate
over-counts. Upstream corrects it when the turn actually runs (startLocked recomputes),
so the damage is limited to the status API showing a wrong `turn` for queued runs — but
if your fork's client uses the queued turn for optimistic ids, either recompute on
dequeue before exposing it, or document the field as approximate and never derive ids
from a queued handle.

**(d) Polymorphic response envelope.**
`GET /v1/threads/{id}/messages` returns a bare array when settled and
`{data, live}` when live (`threads.go:296-305`). Every client must branch on shape, and
the terminal-window fold (which returns the bare array even though it folded) makes the
shape non-deterministic from the client's view. The fork may prefer
**always-envelope from day one**: `{data:[...], live: null | {...}}` — one shape, one
parser, and `live:null` still tells the client "nothing to attach to". If you keep
upstream's shape for wire-compat, write the branching client helper once and test both.

**(e) `"auto"` is a bare magic string, and `model:"auto"` without `agent_id` 404s.**
`chat.go:74` compares against the literal `"auto"`. Worse: the auto branch only checks
`explicitAgentID` (from `RouteAgentID()`); a request sending `model:"auto"` with no
`agent_id` falls through `agentID = req.Model` → `registry.Get("auto")` is nil →
`explicitAgentID == ""` so no 404-on-agent → proxy path → `ProxyProvider(cfg, "auto")`
→ upstream "model not found" 404. In the fork: declare `const AgentAuto = "auto"`, and
decide the model-sentinel behaviour explicitly — either treat `model:"auto"` the same as
`agent_id:"auto"` (recommended if your UI ever sends it), or reject it with a clear 400
("auto is an agent selector, not a model"). Do not leave the accidental proxy fallthrough.

**(f) Zero tests shipped across all four PRs.**
The fork should ADD, minimum:
- `NextTurn`: empty log → 0; one flushed turn → 1; open (mid-run) turn → returns the
  OPEN turn (resume semantics); assistant-less terminal does NOT advance; stable across
  re-projection of a grown log.
- Projector folds: `EvQuestion` → dynamic-tool + data-tool-interrupt shapes;
  duplicate EvQuestion for the same toolCallID doesn't duplicate the dynamic-tool part;
  `EvHITLResolved` approved/denied → `resolved` stamped, tool call re-surfaced; missing
  Kind defaults approved.
- `mergeLiveMessages`: upsert-in-place by id preserving archived CreatedAt/Model;
  append-new; stable sort keeps user-before-assistant within the same second.
- `ClassifyAgent`: router returns valid id → picked; router errors → first non-internal
  fallback; router returns garbage → `normaliseAgentID` substring rescue then fallback;
  nil internal registry → fallback.
- The golden live-vs-projection parity test from §2.

---

## 8. Fork adaptation notes

This phase touches whatever custom run-lifecycle your fork has. **Before writing any
code, build this mapping table** — every step above is phrased against upstream names:

| Upstream concept | Upstream symbol | Your fork's equivalent |
|---|---|---|
| Session event log (hot+cold) | `eventlog.CompositeLog`, `coord.Log()` | ? |
| Event fold → messages | `eventlog.ProjectMessages` / `projector` | ? |
| Run coordinator + per-session handle | `agent.Coordinator`, `RunHandle` | ? |
| Run start (fresh turn) | `startLocked` | ? |
| HITL resume | `Coordinator.Resume` | ? |
| Terminal archive flush | `Archiver.flush` | ? |
| AI-SDK encoder | `stream/aisdk.Encoder` | ? |
| Reconnect/attach stream | `StreamSessionAttach` | ? |
| User message persistence | `chat.MessageSaver.SaveUserMessage` | ? |
| Post-run hook seam | `AddPostRunHook` / `PostRunInfo` | ? |
| Thread messages read API | `ThreadsMessagesList` | ? |
| Classifier / router agent | `handler.Route` + internal `"router"` agent | ? |

Watch for these divergence traps specifically:

- **Existing id scheme.** If the fork already assigns message ids (client-generated,
  ULIDs, whatever), you cannot run two schemes: pick the deterministic one and write a
  read-side shim that treats legacy random-id rows as opaque (they merge by id and simply
  never collide). Do NOT rewrite historical ids.
- **Hook signature reuse.** Upstream reuses `PostRunHook`/`PostRunInfo` for start hooks
  (with `Status: RunRunning` and no outcome fields). If your fork's post-run info carries
  outcome-only fields, either introduce a distinct `RunStartInfo` or document which
  fields are zero at start — silent zero-values in a shared struct are a foot-gun.
- **Encoder construction order.** Any fork code path that constructs the stream encoder
  before the run is registered must be inverted (§3 step 5). Grep every constructor call
  site — upstream had five (`stream.go` ×2, `stream_coordinator.go` ×2, `chat.go` proxy).
- **Part shape parity is against YOUR live encoder,** not upstream's. When porting the
  EvQuestion/EvMetadata folds, diff against the frames your fork's encoder actually
  emits today (your interrupt frame may carry extra fields — fold them too).
- **Preserve local changes.** If your fork's `ThreadsMessagesList` already has custom
  filtering/pagination, layer the live fold AFTER the archived-rows query but BEFORE
  serialisation, and make sure pagination doesn't cut the in-progress turn out of the page
  the client lands on (upstream has no pagination — size 1000).

Suggested port order (each step independently shippable, matching PR order):
**#20 → #21 → #22 → #23**, with the §7 fixes folded into #20 (a, b, c), #21 (d), and
#23 (e), and tests (f) landing with each step, not at the end.

---

## 9. End-to-end verification checklist

Run against the fork after all four steps:

1. **Id parity, live vs DB.** Start a 2-turn conversation. For each turn capture the live
   start frame's `messageId` and afterwards `GET /v1/threads/{id}/messages`. Every
   assistant id must be `{thread}:{n}:assistant`, every user id `{thread}:{n}:user`, and
   the live ids must equal the reloaded ids exactly. No duplicate rows.
2. **Return mid-stream.** Start a long run, navigate away, reload the thread mid-stream:
   the page renders the folded in-progress turn immediately (not blank), then the tail
   attach at `live.head_seq` continues mid-sentence with no duplicated or missing text.
3. **Reconnect upsert.** Kill and reopen the SSE connection during a run: the client
   replaces the message in place (same id), no dup-key error, no doubled message.
4. **HITL.** Trigger a question; reload → question card visible. Answer (approve AND deny
   in separate runs); reload → card resolved with the right decision, never re-raised;
   the continuation streamed under the same assistant id.
5. **"Thought for Ns" survives reload** with the same duration as live; footer shows
   model · agent · duration after reload, for both a settled thread and a mid-run reload.
6. **Reload at run end** (within the archive-flush window) shows the complete final turn.
7. **Auto routing.** `agent_id:"auto"` + "Research X in depth" → deep-research;
   `agent_id:"auto"` + "hi" → basic agent; router down → fallback agent, no error.
   `model:"auto"` behaves per your (e) decision — no accidental proxy 404.
8. **Regression guard.** Legacy non-coordinated paths (if the fork still has them) and
   the plain-model proxy still work: no `messageId` on their start frames, random user
   doc ids, unchanged wire output.
9. **Load sanity for (a).** On a session with a large log (thousands of events), starting
   a new turn must not block other sessions' starts — verify with two concurrent sessions
   and an artificially slowed cold store if you can.

Build gates: `go build ./... && go vet ./...` clean, plus the new §7(f) tests green.
