# 08 — Frontend Foundation: Identity Seam, Sessions Client, Reconnect, Question Cards, Live Sidebar, Stop-Cancel

**Repo this doc describes:** `agentui` (Next.js / React chat UI). All code refs below are
paths in the agentui repo at `main` (`a5c544e`); the pre-migration base was `8cc25d0`.

**Scope:** agentui PRs **#1** (`fix/w6-frontend`, merge `b2defb1`, commits `a44e918 →
0aa6ae1`), **#2** (`89c1aaf`, sidebar live refresh), **#3** (`1440367`, stop-cancel).
This is the foundation layer — every later frontend doc (09+) builds on the seams
introduced here.

**Audience:** a coding agent porting these features into a **diverged fork** of agentui
that has its own local changes (possibly its own auth, theming, and layout). Port
intent and architecture, not diffs verbatim. Where the fork differs, the "Fork
adaptation" sections say what the actual swap point is.

**Backend dependencies (from docs 01–07 — the fork's backend must have these first):**

| Feature | Backend API required | Covered in |
|---|---|---|
| Identity seam | `X-User-ID` header respected on every endpoint (sessions, threads, chat, resume, memory scoping) | doc 01/02 |
| Sessions client / reconnect | `GET /v1/sessions`, `GET /v1/sessions/{id}`, `GET /v1/sessions/{id}/stream?format=aisdk&after=N` (replay-then-live, `[DONE]`-terminated per run) | doc 01 |
| Question cards | `question` tool emitting `data-tool-interrupt` with `questions[]`; `POST /v1/agent/resume?format=aisdk` accepting `{thread_id, action, answers, text}` | doc 05 |
| Full-parts rehydration | `GET /v1/threads/{id}/messages` persisting a structured `parts` array per assistant message (same part `type` names as the live stream) | doc 03 |
| Async titles (PR #2) | Backend-side thread creation on first user message + async title generation on `GET /v1/threads` | doc 03 |
| Stop button (PR #3) | `POST /v1/sessions/{id}/cancel` → `{cancelled:true, status:"cancelled"}`, 404 when already settled | doc 01 |

If the fork ports frontend before the matching backend API exists, each feature
degrades (the query hooks resolve to empty/null) but nothing crashes — that is by
design, see Known issue (f) for the one place this defensive posture goes too far.

---

## 1. Architecture: how the UI talks to the backend

Two base URLs, two protocols:

- **`lib/api/client.ts`** — `BACKEND_URL` (`NEXT_PUBLIC_BACKEND_URL`, default
  `http://localhost:8000`). Plain JSON REST via `apiFetch<T>()` for agents, models,
  threads, history, sessions. Direct browser → Go backend (wide-open CORS), **no Next
  proxy**. `apiFetch` deliberately only sets `Content-Type` when there is a body —
  adding it to GETs makes them non-simple and triggers a CORS preflight the backend
  405s.
- **`lib/chat/api.ts`** — `AGENTIC_URL` (`NEXT_PUBLIC_AGENTIC_URL`, default
  `http://localhost:8011`). Streaming endpoints. The Go backend emits the **native
  Vercel AI SDK v6 UI Message Stream** when `?format=aisdk` is present, so the browser
  consumes it directly with `@ai-sdk/react` / `readUIMessageStream` — again no
  translation proxy. Three URL builders:
  - `chatUrl()` → `POST /v1/chat/completions?format=aisdk` (primary transport)
  - `resumeUrl()` → `POST /v1/agent/resume?format=aisdk` (HITL continuation)
  - `sessionStreamUrl(sessionId, afterSeq)` →
    `GET /v1/sessions/{id}/stream?format=aisdk&after=N` (attach/replay a background run)

**Stream part vocabulary** (`lib/chat/types.ts`, `ChatDataParts`): besides native AI-SDK
parts (`text`, `reasoning`, `dynamic-tool`), the backend emits custom data parts, each
appearing on the wire and in `message.parts` as `data-<key>`:

- `data-agent-step` — sub-agent lifecycle (started/done, durationMs) for swarm cards
- `data-agent-delta` — incremental sub-agent token deltas (kind: reasoning|text)
- `data-agent-progress` — transient status line, not persisted
- `data-task-list` — todo/task board snapshot (keyed `id`, replaces prior)
- `data-artifact` — generated artifact (code/doc/file) rendered as a card
- `data-tool-interrupt` — HITL pause: `{toolCallId, toolName, prompt?, details?,
  threadId?, resolved?, questions?, answers?, answerText?}`
- `data-usage` — exact token usage for the context ring

**Tool interrupts** are the HITL mechanism: the run pauses server-side
(`awaiting-input`), the stream carries a `data-tool-interrupt` part, the UI renders
either a generic approve/deny card (merged into the matching `dynamic-tool` ToolCard by
`components/chat/messages/message-list.tsx`) or — when `toolName === "question"` or
structured `questions` are present — the interactive `QuestionCard`. Resolution POSTs
to `resumeUrl()`; the response is *another* v6 stream which the client folds back into
the conversation by upserting messages on their ids
(`components/chat/use-interrupt-resolver.ts`).

**Stream plumbing helper:** `lib/chat/sse-to-chunks.ts` — parses an SSE body
(`data: {json}\n\n`) into `UIMessageChunk`s and **closes at the `[DONE]` sentinel**.
This matters: the session-follow endpoint keeps its HTTP response open across turns, so
without closing at `[DONE]` a lingering reconnect reader would merge a *later* turn's
deltas into a finished message. Port this file verbatim.

**Key invariant — session_id === thread id.** A persisted thread's background run is
keyed by the same id the thread uses. Reconnect, cancel, and the sidebar status all
rely on this equality.

---

## 2. File inventory

New files (create in the fork):

| File | Role |
|---|---|
| `lib/api/sessions.ts` | `SessionHandle` type, `cancelSession()`, `useSessions()`, `useSessionStatus()`, `useMarkViewed()` (markViewed arrives with doc 09's backend viewed-flag; harmless to include now) |
| `components/chat/use-session-reconnect.ts` | attach to a still-running background run on reload |
| `components/chat/messages/question-card.tsx` | interactive question form + `extractQuestions` / `isQuestionInterrupt` helpers |

Modified files:

| File | Change |
|---|---|
| `lib/api/client.ts` | `getUserId()` + `X-User-ID` on `apiFetch` |
| `lib/chat/api.ts` | `sessionStreamUrl()` helper |
| `lib/chat/types.ts` | `Question` type; `tool-interrupt` grows `questions/answers/answerText` |
| `lib/chat/from-history.ts` | full-parts rehydration (`rehydratePart`) |
| `components/chat/use-agent-chat.ts` | `X-User-ID` on the chat transport; PR #2 `onFinish` invalidate |
| `components/chat/use-interrupt-resolver.ts` | `X-User-ID` on resume; `answers`/`text` passthrough |
| `components/chat/interrupt-context.ts` | `ResolveInterrupt` gains optional `InterruptAnswers` arg |
| `components/chat/thread-view.tsx` | wire reconnect hook; PR #3 `handleStop` + `stopped` state |
| `components/chat/messages/message-list.tsx` | route question interrupts to QuestionCard; suppress the raw `question` ToolCard; pass `stopped` |
| `components/chat/messages/run-progress.tsx` | PR #3 amber "Stopped" badge |
| `lib/api/threads.ts` | PR #2 `refetchInterval: 5000` on `useThreads` |
| `components/sidebar/app-sidebar.tsx` | **see the SKIP note in 3.5 — do not port PR #1's version** |

---

## 3. PR #1 — W6 foundation (`8cc25d0..b2defb1`)

Six independent features; port in this order (each is a checkpoint).

### 3.1 Identity seam — `getUserId()` + `X-User-ID` everywhere

Intent: the backend keys **all** per-user state (sessions, resume matching, memory,
MCP tokens) by `X-User-ID`. Every HTTP call the UI makes — REST *and* all three
streaming paths — must carry the **same** id, or e.g. the chat stream runs as a
different backend user than the sessions list and reconnect/resume silently never
match.

`lib/api/client.ts`:

```ts
export function getUserId(): string {
  if (typeof window !== "undefined") {
    return window.localStorage.getItem("agentic_user_id") ?? "anonymous";
  }
  return "anonymous";
}
// in apiFetch:
if (!headers.has("X-User-ID")) headers.set("X-User-ID", getUserId());
```

The header is then hand-set in three more places (upstream state — the port should
centralize this instead, see Known issue (d)):

1. `components/chat/use-agent-chat.ts` — `prepareSendMessagesRequest` returns
   `headers: { ...headers, "X-User-ID": getUserId() }` for the chat stream.
2. `components/chat/use-interrupt-resolver.ts` — on the `fetch(resumeUrl(), …)` call.
3. `components/chat/use-session-reconnect.ts` — on the session-stream `fetch`.

Note `use-agent-chat.ts` also maps the camelCase request body to the backend's
snake_case (`agent_id`, `thread_id`, `use_rag`, `reasoning_effort`) inside
`prepareSendMessagesRequest` — if the fork's transport differs, that mapping plus the
identity header are the two things that must survive.

**Checkpoint:** DevTools → Network: every request to both base URLs carries
`X-User-ID` with the same value.

### 3.2 Sessions API client — `lib/api/sessions.ts`

Port the whole file. Core type (worth reproducing — later docs depend on every field):

```ts
export interface SessionHandle {
  session_id: string;   // === thread id
  user_id: string;
  agent_id: string;
  status: "queued" | "running" | "awaiting-input" | "done" | "error" | "cancelled";
  viewed: boolean;      // used by doc 09's sidebar ring; backend sends it from doc 01
  start_seq: number;    // first event-log seq THIS run writes (reconnect fallback)
  turn: number;         // 0-based assistant turn — keys the deterministic message id
  started_at: string;
  updated_at: string;
}
```

- `useSessions()` — `["sessions"]` query, `refetchInterval: 5000`, `retry: false`.
- `useSessionStatus(id, enabled)` — single-session probe, **gated by `enabled`**:
  `GET /v1/sessions/{id}` 404s for settled threads, and callers who already know
  (from the sessions list) that a thread has no active run pass `enabled: false` to
  avoid a console 404 on every thread load (commit `b90b542` added this gate —
  build it in from the start).
- Add `isActiveStatus` here per Known issue (e).

**Checkpoint:** with a run in flight, `useSessions()` data shows the session
`running`; after it finishes, status flips within 5s.

### 3.3 `useSessionReconnect` — rejoin a background run on reload

Intent: runs execute server-side decoupled from the connection. If the user reloads
(or the tab dropped) mid-run, the thread view must re-attach to
`GET /v1/sessions/{id}/stream` and keep painting. Deterministic message ids
(`{session_id}:{turn}:assistant`, doc 04) make the merge an idempotent upsert.

**Port the PR #1 shape first** (`git show b2defb1:components/chat/use-session-reconnect.ts`
in agentui): full replay with `after=0`, upsert-by-id merge. The current-main version
adds tail-only attach (`liveHeadSeq` from a session-aware history fetch, `mergeTailParts`
splicing, orphan tool-output handling, throttled flush) — that arrives with PR #11 /
doc 10 and **requires backend session-aware history**. Don't build it yet; do keep the
hook's signature ready for an optional 4th arg.

The three guards in this hook are the hard-won part — port them exactly:

1. **Gate the probe on the sessions list** (`maybeLive`): only call
   `useSessionStatus(threadId, maybeLive)` when the polled `/v1/sessions` list shows
   this thread active — avoids a 404 probe on every settled-thread load.
2. **`primaryOwnedRef` latch:** once the primary `useChat` transport has *ever* been
   `streaming`/`submitted` in this mount, this client owns the run — reconnect must
   never attach, even in the windows where `status` drops to `ready` (at an interrupt,
   or right after a turn ends) while the session is still listed live. Without the
   latch you get a duplicated assistant bubble / question card and a race against the
   interrupt resolver (commit `0aa6ae1` fixed exactly this).
3. **`attachedRef` attach-once** + `AbortController` cleanup on unmount.

```ts
const primaryOwnedRef = useRef(false);
if (primaryBusy) primaryOwnedRef.current = true;
// effect: if (!isLive || primaryBusy || primaryOwnedRef.current || attachedRef.current) return;
```

Wire it in `ThreadChat` (`components/chat/thread-view.tsx`):
`const { reconnecting } = useSessionReconnect(threadId, status, setMessages)` — and
only for persisted threads (temporary chats have no server session). `reconnecting`
can drive a small "reattaching…" indicator; agentui shows the stream itself.

**Checkpoint:** start a long run, reload mid-stream → the transcript repaints and
continues live; no duplicate assistant message. Send a message and stay on the page →
Network shows **no** request to `/v1/sessions/{id}/stream` (latch working).

### 3.4 Interactive QuestionCard + resume `answers`

Backend contract (doc 05): the `question` tool pauses the run with a
`data-tool-interrupt` whose payload carries structured questions (opencode schema —
either at `data.questions` or nested at `data.details.questions`; accept both). The
answer round-trips through the resume endpoint as **selected option labels**:
`{thread_id, action: "approved", answers: string[][], text?: string}`.

Type additions in `lib/chat/types.ts`: `Question` (`question`, `header`, `options:
{label, description?}[]`, `multiple?`, `custom?`) and the `tool-interrupt` part gains
`questions? / answers? / answerText?`.

`components/chat/interrupt-context.ts`: `ResolveInterrupt` gains an optional third
argument `InterruptAnswers = { answers?: string[][]; text?: string }`.

`components/chat/use-interrupt-resolver.ts`: on resolve, (1) optimistically stamp
`resolved: action` **plus** `answers`/`answerText` onto the interrupt part via
`setMessages` so the card settles instantly, (2) POST them to `resumeUrl()`, (3) fold
the continuation stream back in (existing upsert logic; unchanged).

`components/chat/messages/question-card.tsx` — export three things:

- `extractQuestions(interrupt)` — reads `questions` from either location, returns
  `Question[] | null`.
- `isQuestionInterrupt(interrupt)` — `toolName === "question" || extractQuestions() != null`.
- `QuestionCard` — pending: option rows (radio single / checkbox `multiple`),
  optional free-text row (`custom` defaults **true**), Submit resolves
  `("approved", { answers, text })`, folding each question's trimmed free text into
  its answer list. Settled: read-only recap of chosen answers.

State model (this is the part that must be built correctly now; the *visuals* get
reworked later): `selected: string[][]` (labels per question) + `texts: string[]`,
seeded from `questions`. Apply Known issues (a) and (b) here — the shipped seeding is
buggy when questions arrive after mount.

Routing in `components/chat/messages/message-list.tsx` (commit `10b52ec`):

- `case "data-tool-interrupt"`: if `isQuestionInterrupt(part.data)` render
  `<QuestionCard interrupt={part.data} />`; otherwise keep the existing behavior
  (interrupt merged into the matching ToolCard as approve/deny).
- `case "dynamic-tool"`: **skip the raw ToolCard when `part.toolName === "question"`**
  — otherwise the same interrupt renders twice (interactive card + a redundant generic
  approve/deny card, because the tool part sits in `approval-requested` state with no
  interrupt of its own).

Note for the fork: PR #5 later moves the *active* question card into the composer slot
and PR #14 adds a multi-question pager (doc 09/10). Rendering it inline in the
transcript is correct for this stage; keep the `variant` concept out until doc 09.

**Checkpoint:** trigger the `question` tool → exactly one interactive card;
selecting options + Submit resumes the run and the card flips to the settled recap;
a `write_database`-style interrupt still shows the plain approve/deny card.

### 3.5 Sessions sidebar — **SKIP the separate "Running" section**

PR #1 added a `RunningSessions` sidebar group (its own `SidebarGroup` labelled
"Running", one pulsing-dot row per active session, linking to `/chat/{session_id}`).
**PR #8 (`a52f468`) deleted it** and integrated a 4-state status icon directly into
the existing thread rows instead (green pulse running / exclamation awaiting-input /
ring for unviewed-terminal / nothing). Reasons for the removal, so the fork doesn't
re-litigate: the separate section duplicated the same thread in two lists, rows showed
`agent_id` instead of the thread title, and it caused layout shift whenever a run
started/finished.

**Fork instruction: do not build `RunningSessions` at all.** Jump straight to the
integrated thread-row status from doc 09. What you *do* need from this PR is already
done in 3.2: `useSessions()` polling every 5s — the sidebar consumer changes, the data
source doesn't. If you want an interim signal before doc 09, a minimal
`isActiveStatus(session.status)` dot on the existing `ThreadRow` is a strict subset of
doc 09's work; the current-main reference implementation is
`components/sidebar/app-sidebar.tsx` (see `isActiveStatus` usage and the status-icon
block around lines 260–280).

### 3.6 `fromHistory` full-parts rehydration (commit `54c68f4`)

Intent: the backend persists assistant messages with the **same structured `parts`
array** the live stream produced (same `type` names). On reload, reconstruct those
parts so a reloaded message renders identically to a live-streamed one — tool cards,
reasoning collapsible, agent cards, task board, artifacts — instead of collapsing to a
text blob.

`lib/chat/from-history.ts` — `rehydratePart(raw)` is a defensive normalizer:

- `text` → pass through; `reasoning` → keep `startedMs`/`endedMs` (real "Thought for
  Ns" on reload; the fields land with doc 06's backend, harmless before).
- `dynamic-tool` → require `toolName` + `toolCallId` (else drop), default `state` to
  `"output-available"` so settled cards render settled; keep `input/output/errorText`.
- All `data-*` types listed in §1 → require a record `data`, preserve `id` (consumers
  key/aggregate on it).
- Unknown types → pass through if they look renderable (`text` or `data` present),
  else drop. **Never throw** — a malformed persisted part must not kill the reload.

`fromHistory(messages)`: filter to user/assistant, use persisted `parts` when present
and at least one part survives rehydration, else fall back to a single text part from
`m.content` so a bubble is never empty. (The `messageMetadata` footer block in
current main is doc 12's concern — skip it here unless the fork's backend already
persists `model/agent_id/duration_ms`.)

**Checkpoint:** run a turn with tool calls + reasoning, reload → tool cards and the
reasoning collapsible reappear identical; a legacy text-only thread still renders.

---

## 4. PR #2 — sidebar live refresh (`89c1aaf`)

Problem: thread creation is **backend-side on the first user message**, so a brand-new
chat's thread doc only exists in `/v1/threads` after the run — and the sidebar list
(`["threads"]` query) was fetched only on mount, showing "No conversations yet." until
a hard reload. Titles are generated **async** a few seconds after the run, so a single
invalidation isn't enough either.

Dual mechanism, both halves required:

1. `components/chat/use-agent-chat.ts` — in the Chat `onFinish` callback:
   `queryClient.invalidateQueries({ queryKey: ["threads"] })` → the new thread appears
   immediately (provisional "New Chat" title).
2. `lib/api/threads.ts` — `useThreads()` gains `refetchInterval: 5000` (mirrors
   `useSessions`) → the async generated title converges live a few seconds later.

Fork adaptation: if the fork renamed the query key, target its equivalent —
the semantic is "invalidate the sidebar list query on chat finish". If the fork
already has an `onFinish`, append; don't replace (agentui's also captures usage).

**Checkpoint:** new chat → send → on finish the sidebar shows the thread at once;
within ~5–10s the title changes from the provisional one to the generated one, no
reload.

---

## 5. PR #3 — Stop cancels the server run (`1440367`)

Problem: the composer's Stop button was `onStop={stop}` — an AI-SDK **client-side
abort only**. The background server run kept executing (observed orphaned >60s) and
`/v1/sessions` stayed `running`. Worse, `RunProgress` derived its badge from
`streaming && isLast`, and a client abort drops `status` to `ready` exactly like a
natural finish — so a stopped run showed a false green "Completed".

Three pieces:

1. `lib/api/sessions.ts` — `cancelSession(id)`: `POST /v1/sessions/{id}/cancel` via
   `apiFetch` (so X-User-ID rides along); swallows `ApiError` 404 (run already
   settled — a benign race), rethrows anything else.
2. `components/chat/thread-view.tsx` — `handleStop` + `stopped` state:

```ts
const [stopped, setStopped] = useState(false);
const handleStop = useCallback(() => {
  stop();                       // abort the client stream
  setStopped(true);
  if (!isTemporary) {           // temporary chats have no server session
    void cancelSession(threadId).finally(() => {
      queryClient.invalidateQueries({ queryKey: ["sessions"] }); // sidebar flips now, not in 5s
    });
  }
}, [stop, isTemporary, threadId, queryClient]);
```

   ⚠ Port this **with** the `.catch` fix from Known issue (c) — as shipped, a non-404
   cancel failure becomes an unhandled promise rejection.

   `setStopped(false)` at the top of `onSubmit` so the next turn clears the flag.
   `stopped` is threaded through `MessageList` to `RunProgress` (applied to the last
   assistant message only — `message-list.tsx` passes `stopped={stopped && isLast}`
   semantics via its existing plumbing).
3. `components/chat/messages/run-progress.tsx` — three-way badge: streaming →
   pulse "Running · N steps"; `stopped` → amber `Ban` icon "Stopped · N steps";
   else → green `Check` "Completed · N steps". A natural finish is unaffected.

**Checkpoint:** start a long run, click Stop → Network shows the cancel POST returning
200 `{cancelled:true,status:"cancelled"}`; `/v1/sessions` flips to `cancelled` (watch
the 5s poll or the invalidation); badge reads amber "Stopped", never "Completed";
composer is immediately usable; next send shows a normal run and a green "Completed".

---

## 6. Known issues — fix during port

These are real defects in the merged agentui code. The fork should land the fixed
versions directly; do not reproduce the bugs for fidelity.

**(a) QuestionCard `toggle` crashes if questions arrive after mount.**
`selected`/`texts` are seeded once via `useState(() => questions.map(...))`. If the
card first renders while `questions` is empty (interrupt part streams in before the
questions payload, or a later stream update grows the array), `selected` stays `[]`
and `toggle` does `next[qi].indexOf(label)` on `undefined` → TypeError. Fix both ends:

```ts
const cur = next[qi] ?? (next[qi] = []);          // in toggle
useEffect(() => {                                  // re-seed on shape change
  setSelected((prev) => questions.map((_, i) => prev[i] ?? []));
  setTexts((prev) => questions.map((_, i) => prev[i] ?? ""));
}, [questions.length]);
```

**(b) Empty/missing `questions` on a question interrupt renders nothing — run stuck.**
`QuestionCard` returns `null` when `questions.length === 0`, and `message-list.tsx`
routes an interrupt with `toolName === "question"` to QuestionCard even when
`extractQuestions` returned null (`isQuestionInterrupt` is an OR). Result: the backend
sits `awaiting-input` with **no UI to resolve it**. Fix in the router: only take the
QuestionCard branch when `extractQuestions(part.data) != null`; a `question` interrupt
with no parseable questions must **fall back to the generic approve/deny card** so the
user can always unblock the run.

**(c) `cancelSession` rethrows into a `.finally` with no `.catch`.**
`cancelSession` deliberately rethrows non-404 errors, but `thread-view.tsx` calls
`void cancelSession(threadId).finally(...)` (line ~122 in current main) — `.finally`
doesn't handle rejection, so any 5xx/network failure is an unhandled rejection. Add
`.catch((err) => console.warn("cancel failed", err))` (or surface a toast) before the
`.finally`. Keep the rethrow in `cancelSession` itself — other callers may care.

**(d) Build `identityHeaders()` from day one.**
`X-User-ID` is hand-set in three hooks besides `apiFetch` (§3.1). In the fork, add to
`lib/api/client.ts`:

```ts
export function identityHeaders(): Record<string, string> {
  return { "X-User-ID": getUserId() };
}
```

and spread it at all four sites. This is also the fork's auth swap point — see §7.

**(e) Hoist the active-status triple.**
`status === "running" || "awaiting-input" || "queued"` is duplicated in
`use-session-reconnect.ts` (`maybeLive`), `app-sidebar.tsx` (`isActiveStatus` — main
did eventually hoist a local copy there), and PR #1's `RunningSessions`. Define once in
`lib/api/sessions.ts`:

```ts
export const isActiveStatus = (s: SessionHandle["status"]) =>
  s === "running" || s === "awaiting-input" || s === "queued";
```

and import it everywhere. Doc 09's sidebar work reuses it.

**(f) Query fns swallow ALL errors, hiding backend failures.**
`useThreads`, `useSessions`, `useThreadMessages` wrap their fetch in
`try { … } catch { return [] }` — a misconfigured `BACKEND_URL`, CORS break, or 500
renders as a permanently empty sidebar with zero signal, indistinguishable from a new
user. Keep the *intended* soft cases (404 on `useSessionStatus` = settled thread →
`null`) but let real errors reach React Query (`retry: false` already bounds noise):

```ts
queryFn: async () => unwrap(await apiFetch<...>("/v1/threads")),
```

and render `query.isError` as a small "couldn't load — retry" row instead of the empty
state. During the port this also stops backend-dependency gaps (docs 01–07 not yet
merged in the fork) from masquerading as "works, just empty".

---

## 7. Fork adaptation notes

- **Auth:** the entire identity design funnels through `getUserId()` in
  `lib/api/client.ts` — a localStorage-or-"anonymous" placeholder. If the fork has real
  auth (SSO/JWT/cookies), replace `getUserId()`'s body (or `identityHeaders()` per
  issue (d)) with the fork's subject/token and you are done — nothing else references
  identity. If the fork's backend authenticates via cookie/Authorization instead of
  `X-User-ID`, make `identityHeaders()` return the right header(s) and add
  `credentials: "include"` in `apiFetch` + the three stream fetches; the seam holds.
- **Base URLs / proxying:** agentui talks to the backend directly from the browser
  (CORS open). If the fork proxies through Next route handlers, keep the *paths* and
  the `?format=aisdk` query intact and forward the identity header through the proxy.
- **Query keys:** `["threads"]`, `["sessions"]`, `["session-status", id]`,
  `["thread-messages", id]`. If the fork's keys differ, map the invalidations
  (PR #2 `onFinish`, PR #3 cancel) onto its keys — the semantics, not the strings.
- **Chat transport:** agentui owns a `Chat` instance in `use-agent-chat.ts`
  (`useMemo` keyed by thread id) so secondary subscribers share it, with
  `experimental_throttle: 50`. If the fork uses plain `useChat`, the ports still work —
  the touch points are `prepareSendMessagesRequest` (identity + snake_case body) and
  `onFinish` (invalidate). AI SDK v6 is assumed; on v4/v5 the
  `readUIMessageStream`/`UIMessageChunk` APIs differ and `use-session-reconnect` +
  `sse-to-chunks` need the v6 upgrade first.
- **Component library:** cards use shadcn/Tailwind tokens; restyle freely. The
  contracts that must survive restyling: QuestionCard's submit payload
  (`answers: string[][]` of **labels** + optional `text`), the settled-state recap
  driven by `interrupt.resolved/answers/answerText`, and RunProgress's three-way
  streaming/stopped/completed distinction.
- **Ordering vs later docs:** doc 09 (sidebar thread-row status, question composer
  slot) and doc 10 (tail-only reconnect, swarm streaming) assume this doc's seams
  exist under these names — if the fork renames files, keep the exports
  (`isActiveStatus`, `extractQuestions`, `isQuestionInterrupt`, `cancelSession`,
  `useSessions`, `sessionStreamUrl`) discoverable.

---

## 8. Verification (manual browser pass, in order)

Prereqs: fork backend running with docs 01–07 merged; `NEXT_PUBLIC_BACKEND_URL` /
`NEXT_PUBLIC_AGENTIC_URL` pointing at it. Run `tsc --noEmit` and lint after each PR
block.

1. **Identity:** open the app, send a message. Network tab: `/v1/chat/completions`,
   `/v1/threads`, `/v1/sessions` all carry the same `X-User-ID`. Change the stored id
   (or log in as another user), reload → previous threads are gone (scoping works).
2. **Reconnect:** start a multi-step run, hard-reload mid-stream → transcript repaints
   from history and keeps streaming; exactly one assistant bubble. Stay on page for a
   full run → no `/stream` request ever fires from this tab.
3. **Question card:** trigger the `question` tool. One interactive card; multi-select
   honors `multiple`; free text alone enables Submit; Submit resumes and the card
   settles with the chosen labels. Regression (issue b): simulate an empty
   `questions` interrupt → approve/deny card appears, run is resolvable.
4. **Rehydration:** after a tool-heavy turn, reload → tool cards (settled state),
   reasoning collapsible, task board, artifacts all render as they did live.
5. **Sidebar (PR #2):** brand-new chat → on finish the thread appears instantly with a
   provisional title; generated title lands within ~10s; second thread via soft-nav
   behaves the same.
6. **Stop (PR #3):** long run → Stop. Cancel POST returns 200; session status
   `cancelled`; amber "Stopped · N steps" badge (not green Completed); composer
   usable; a subsequent natural finish still shows green "Completed". Also verify
   Stop on a **temporary** chat sends no cancel request.
7. **Error surfacing (issue f):** point `NEXT_PUBLIC_BACKEND_URL` at a dead port →
   sidebar shows an error/retry state, not a silent "No conversations yet."
