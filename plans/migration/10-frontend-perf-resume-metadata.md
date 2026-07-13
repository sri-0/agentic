# 10 — Frontend: perf comparators, tail-only resume, metadata rehydration, Auto agent, question pager

**Repo:** `agentui` (Next.js / React). Reference: merged `main` @ `a5c544e`.
**Reference PRs:** #9 `f3631ba`, #10 `c948cb5`, #11 `ec4ccee`, #12 `8d85a5b`, #13 `ce88eca`, #14 `a4fefc1`.
**Prerequisites:**

- Frontend docs **08** (identity seam, sessions API, reconnect hook v1, question card) and
  **09** (swarm task bar, sub-agent panel, question-replaces-composer, thread-row status)
  fully ported first — every section below edits files those docs created.
- Backend doc **07 is a HARD dependency**. Nothing in this doc works against a backend that
  lacks it; this doc consumes, and silently misbehaves without:
  - **Deterministic message ids** `{session}:{turn}:{role}` stamped identically on live start
    frame, replay projection, and archived doc (PRs #10/#11);
  - **Session-aware messages API** — `GET /v1/threads/{id}/messages` returning
    `{data, live:{head_seq,turn,status}}` while a run is active (PR #11);
  - **`start_seq` / `turn` on the session status** handle (replay fallback, PR #10);
  - **Metadata + reasoning-timing persistence** — `model`/`agent_id`/`duration_ms` on
    archived messages, `startedMs`/`endedMs` on reasoning parts (PR #12);
  - **The Auto agent classifier** accepting `agent_id: "auto"` (PR #13).

  Do not start until backend doc 07's verification checklist passes against your fork's
  backend — porting this against a doc-06-level backend fails not at compile time but as
  duplicated turns on reload, blank resumes, and a picker default the backend rejects.

**What this phase delivers:** the swarm stream stops melting the main thread (memo
comparators + memoized scans: 175→0 re-renders per component, ~1 FPS → ~120); navigating away
from a mid-stream swarm and back repaints in <300ms from a server-folded snapshot plus a
tail-only live attach; reload shows the real "Thought for N seconds" and the
model · agent · duration footer; the picker gains a classifier-routed "Auto" default; the
multi-question HITL card becomes a paged, option-row form.

---

## 0. The resume/identity contract (recap — internalize before touching code)

Everything in PRs #10–#12 hangs off three backend-guaranteed invariants from doc 07:

1. **Message identity.** Every message id is `{session_id}:{turn}:{role}` (e.g.
   `th_abc:3:assistant`), stamped identically on (a) the live SSE start frame, (b) the
   `?after=` replay projection, and (c) the persisted history doc. Consequence: *upserting on
   `id` is a true idempotent replace* — same turn ⇒ same id ⇒ replace, new turn ⇒ new id ⇒
   append. Every merge in this doc (reconnect upsert, tail splice, HITL resume) leans on this.
   If your fork's backend generates random message ids, stop and port doc 07 first.

2. **The `{data, live}` history envelope.** `GET /v1/threads/{id}/messages` returns a bare
   array for a settled thread; while the session is ACTIVE it returns
   `{data: ThreadMessage[], live: {head_seq, turn, status}}` where `data` already contains
   the in-progress assistant turn **fully folded server-side** up to seq `head_seq`. Fetch
   and fold point are one atomic snapshot — that makes the tail attach gap-free by
   construction.

3. **The `after=` attach point.** `GET /v1/sessions/{id}/stream?after=N` replays events with
   seq > N, then goes live, and emits `[DONE]` at end-of-run (the HTTP response itself stays
   open across runs — §2.3). Two attach modes:
   - **Tail-only** (preferred): `after = live.head_seq` from the same history snapshot that
     seeded `initialMessages` — only NEW events stream, spliced onto the folded base
     (`mergeTailParts`, §3.3).
   - **Turn replay** (fallback when the fetch carried no `live`, e.g. the run started between
     fetch and attach): `after = max(0, start_seq - 1)` replays exactly the in-progress turn;
     deterministic ids make it an idempotent replace.

Keep the trichotomy straight: **history fetch** paints finished turns + the folded current
turn; **tail stream** appends only what happened after the fold; **turn replay** is the
safety net. The client never replays the whole session log (`after=0`) anymore — that was
the doc-08 design; PRs #10/#11 retire it.

---

## 1. PR #9 (`f3631ba`) — sidebar ring, agent-card cleanup, usage ring on `onFinish`

### 1.1 Intent

Three small refinements to doc-08/09 surfaces. If your fork followed doc 09's SKIP directive
it never built `AgentLivePreview` (upstream built it and this PR tears it out) — the PR then
reduces to the sidebar sizing/positioning change and the usage-ring rework; do not go hunting
for preview code to delete.

### 1.2 Sidebar: crisp ring + status icon in the trash slot (`components/sidebar/app-sidebar.tsx`)

- `CompletedRing`: `size-2 border-[1.5px]` → `size-2.5 border-2`. Sub-pixel borders render
  blurry at typical DPRs; a 2px border on a 10px circle is crisp.
- The per-row status icon (pulse dot / amber `CircleAlertIcon` / CompletedRing — the 4-state
  priority from doc 09) moves **out of the `<Link>` inside `SidebarMenuButton`** to an
  absolutely positioned sibling:

  ```tsx
  {session && (
    <span className="pointer-events-none absolute right-1 top-1/2 -translate-y-1/2
                     transition-opacity group-hover/menu-item:opacity-0">
      {/* 4-state priority icon unchanged */}
    </span>
  )}
  ```

  Rationale: the hover-revealed `SidebarMenuAction` (trash) occupies `right-1`; putting the
  status icon in the SAME absolute slot with `group-hover:opacity-0` makes the trash swap in
  with **zero layout shift** (upstream measured 0px). `pointer-events-none` keeps the link
  clickable. The `group-hover/menu-item` named group comes from shadcn's sidebar primitives —
  match your fork's group name.

### 1.3 Usage ring: defer to `onFinish` (`use-agent-chat.ts`, `thread-usage-ring.tsx`, `thread-view.tsx`)

Doc-08's ring subscribed to the chat instance (`useChat({ chat })`) and recomputed
`deriveRichUsage(messages)` on every throttled chunk (~20x/sec). Replace with capture-once:

- `useAgentChat` gains `lastUsage` state: seeded from `initialMessages` on mount
  (`deriveRichUsage(initialMessages, 128_000)` — the window arg is a placeholder, only totals
  are consumed), updated exactly once per turn in `onFinish: ({messages}) =>
  setLastUsage(deriveRichUsage(messages, 128_000))`. Return `{...helpers, chat, lastUsage}`.
- `ThreadUsageRing` drops the `chat` prop and `useChat` subscription; takes
  `lastUsage: RichUsage | null`, resolves the denominator LIVE from the selected model
  (`models.find(m => m.id === selectedModel)?.context_length ?? 128_000`), passes primitives
  to the memoized shell. `ThreadChat`'s `contextSlot` memo deps become `[lastUsage, onOpenUsage]`.

Result: one re-render per completed turn instead of per chunk, a denominator that follows
model switches, and a correctly seeded ring on reload.

### 1.4 Verification

- Sidebar: hover a status-icon row — trash appears exactly where the icon was, zero shift;
  unhover restores it. Ring crisp at 1x and 2x DPR.
- Usage ring: no ticking during streaming, one jump at finish; on reload it shows the last
  turn's usage immediately.

---

## 2. PR #10 (`c948cb5`) — mid-stream rebuild: fresh history + turn replay + `[DONE]` scoping

### 2.1 Intent

First half of "navigate away mid-stream and back is never blank": with doc 07's deterministic
ids, the reconnect upsert becomes a true idempotent replace, so the client seeds finished
turns from history and replays only the in-progress turn. Touches `thread-view.tsx`,
`use-session-reconnect.ts`, `lib/api/sessions.ts`, `lib/chat/sse-to-chunks.ts`. PR #11 then
supersedes the replay with the tail-only attach but KEEPS everything here as the fallback —
port this PR fully, don't skip to #11.

### 2.2 The three mechanisms

**(a) Fresh-history gate (`thread-view.tsx`).** The Chat instance seeds `initialMessages`
exactly once per mount. On navigate-back to a mid-stream thread, react-query serves a cached
history snapshot that predates the in-progress turn — its user message is missing, so the
thread looks blank/stale. Gate on freshness:

```tsx
if (history.isLoading || !history.isFetchedAfterMount) {
  return ( /* skeleton */ );
}
```

`isFetchedAfterMount` holds the skeleton for one refetch round-trip so the seed includes the
current turn's user message. (Requires the history query to actually refetch on mount —
verify your fork's `useThreadMessages` does not set `staleTime: Infinity`.)

**(b) Forced sessions refetch on mount + turn-replay attach (`use-session-reconnect.ts`).**
The hook's `maybeLive` gate reads the polled `useSessions()` list (5s interval); a run started
moments ago isn't in the cached list yet, so a returning thread sat blank until the next poll
tick. Refetch on mount:

```ts
useEffect(() => {
  if (!threadId) return;
  void queryClient.refetchQueries({ queryKey: ["sessions"] });
}, [threadId, queryClient]);
```

Then attach at the turn boundary instead of `after=0`:

```ts
const startSeq = session?.start_seq;
const after = Math.max(0, (startSeq ?? 1) - 1);   // in the attach effect
const res = await fetch(sessionStreamUrl(threadId, after), { ... });
```

`start_seq` is the first event-log seq THIS run writes (doc 07), so `after = start_seq - 1`
replays exactly the current turn; finished turns came from history. Add `startSeq` to the
effect deps. Keep the doc-08 `primaryOwnedRef` / `attachedRef` double-stream guards verbatim —
they prevent a second reader racing the primary transport at interrupts.

**(c) `SessionHandle` type (`lib/api/sessions.ts`).** Add `start_seq` and `turn` — **OPTIONAL**
(Known issue (e); upstream typed them required, which lies to the compiler about older
backends and serialized handles).

### 2.3 `[DONE]` closes the chunk stream (`lib/chat/sse-to-chunks.ts`)

Upstream's comment states the reasoning; quote it because it is the load-bearing insight:

> The stream CLOSES at the backend's `[DONE]` sentinel (end of the current run). The
> session-follow endpoint keeps its HTTP response open across runs/turns, so without this the
> consumer's read loop would hang on the open connection after the turn finished — and worse,
> a lingering reconnect reader would merge a LATER turn's deltas into the finished message.
> Ending at `[DONE]` scopes each reader to exactly one run.

The concrete corruption: the reconnect reader attaches mid-run, the run finishes, but the
reader stays subscribed to the still-open follow endpoint; the NEXT turn's deltas then arrive
on the OLD reader, which upserts them into the previous assistant message, and later turns
corrupt. The fix:

```ts
if (payload === "[DONE]") {
  closed = true;
  controller.close();
  await reader.cancel().catch(() => {});
  return;
}
```

plus a `closed` flag so `finally` doesn't double-close the controller. Also fixes the
HITL-resume consumer (same parser).

### 2.4 Verification

- Start a long streaming turn, navigate away, return within ~2s: NOT blank; all prior turns +
  the in-progress turn; streams to completion.
- After it finishes, send a follow-up: each turn renders once; **zero** "two children with
  the same key" console errors (the canary for id-identity violations).
- Settled threads: no `GET /v1/sessions/{id}` probe fires (`maybeLive` gate holds).

---

## 3. PR #11 (`ec4ccee`) — THE FPS fix + tail-only session-aware resume

This is the largest and most valuable PR in the phase. Two independent halves.

### 3.1 The memo-comparator pattern (why 175→0)

`agentCardsKey` / `runProgressKey` are **content-signature functions**: a single O(parts) pass
that concatenates exactly the fields the component actually renders into one string. The
pattern:

```
signature(message) = string over {the rendered props, nothing else}
memo(Component, (a, b) => otherProps equal && signature(a.message) === signature(b.message))
```

A swarm turn accumulates ~7,000 `data-agent-delta` parts. Each throttled stream flush (~20/s)
creates a new `message` object, so `memo` with default shallow compare re-renders every
consumer, each doing its own O(parts) scan — multiplied out, ~1 FPS. The signature changes
only when a *rendered* fact changes (an agent appears, a lifecycle step lands, a progress
phase arrives); per-token deltas alter no signature, so the comparator returns `true` and
React skips the subtree. Upstream measured with react-scan: **AgentCards 175→0, RunProgress
175→0 re-renders over a 10s swarm window**, ~1 FPS → ~120.

The upstream bug this PR fixed: the key functions existed (doc 09) but **were never wired
into `memo()`**. The port must wire both:

```tsx
export const AgentCards = memo(function AgentCards({ message, streaming }) { ... },
(a, b) =>
  a.streaming === b.streaming &&
  agentCardsKey(a.message) === agentCardsKey(b.message));

export const RunProgress = memo(function RunProgress({ message, streaming, isLast, stopped }) { ... },
(a, b) =>
  a.streaming === b.streaming &&
  a.isLast === b.isLast &&
  a.stopped === b.stopped &&
  runProgressKey(a.message) === runProgressKey(b.message));
```

Two comparator defects to fix while porting — Known issues (c) and (d) below.

**Second layer: memoize the scans themselves.** The comparator blocks token re-renders, but a
real event still re-renders the card, which would rescan 7k parts. Key the scan's `useMemo` on
the same signature:

```tsx
const cardsKey = agentCardsKey(message);
const agents = useMemo(() => collectAgents(message, streaming), [cardsKey, streaming]);
```

Equivalent in `RunProgress` for the rawSteps → collapsed-steps computation (keyed on
`progressKey`; move `if (totalSteps === 0) return null` AFTER the hook for stable hook
order). eslint's `exhaustive-deps` will flag `message` — suppress with a comment: the
signature IS the dependency. Do not "fix" by adding `message` back; that defeats the memo.

**Third layer: `collectArtifacts` behind `artifactSig` (`thread-view.tsx`).** `ThreadChat`
re-renders per flush (it owns `messages`) and `collectArtifacts(messages)` is
O(messages×parts). Guard it with
`const artifacts = useMemo(() => collectArtifacts(messages), [artifactSig])` where
`artifactSig` is a single cheap pass over the `data-artifact` parts. Upstream keyed this on
a bare **count** — Known issue (b); implement the id+title signature from §7 instead.

### 3.2 Wire types: the `{data, live}` envelope (`lib/api/threads.ts`)

```ts
export interface ThreadLiveInfo { head_seq: number; turn: number; status: string; }
export interface ThreadHistory { messages: ThreadMessage[]; live?: ThreadLiveInfo; }
```

`useThreadMessages`'s `queryFn` returns `ThreadHistory`, accepting both wire shapes: bare
array (settled) → `{messages: res}`; wrapped → `{messages: res.data ?? [], live: res.live}`.
`ThreadView` passes `fromHistory(history.data?.messages ?? [])` and threads
`liveHeadSeq={history.data?.live?.head_seq}` into `ThreadChat` → `useSessionReconnect`.
Cross-cutting fix (00-README #2): don't copy upstream's silent `catch → { messages: [] }` —
log fetch errors at warn.

### 3.3 Tail-only resume (`use-session-reconnect.ts` — study the WHOLE file at `a5c544e`)

The full hook at merged main is ~308 lines; read all of it. Additions over PR #10's version:

**Attach-point selection.** Capture the head seq ONCE at mount (`headSeqRef =
useRef(liveHeadSeq)`) — the Chat instance seeded `initialMessages` from the same snapshot, so
only the captured value is gap-free; a later background refetch reporting a newer head must
NOT move the attach point. Then:

```ts
const headSeq = headSeqRef.current;
const tailOnly = headSeq != null;
const after = tailOnly ? headSeq : Math.max(0, (startSeq ?? 1) - 1);
```

**`mergeTailParts(base, tail, orphanOutputs)`.** In tail mode the stream re-opens the SAME
deterministic message id but carries only post-`headSeq` events, so `readUIMessageStream`
rebuilds a message containing only the tail. The merge splices it onto the folded base
(remembered per-id in a `baseParts` Map, captured from `prev[idx].parts` on first sight).
Three rules, all required:

1. *Supersede*: a `data-*` part with a stable string `id` re-emitted in the tail (task board,
   usage, artifact ids, `agent-step` ids) replaces its base copy — drop the base one so e.g.
   the task board doesn't render twice. Same for a `dynamic-tool` part re-surfaced in the
   tail (HITL resume), matched on `toolCallId`.
2. *Orphan outputs*: apply held-out tool results (below) onto the base's folded tool part —
   `{...p, state: "output-available", output}` or `{...p, state: "output-error", errorText}`.
3. *Boundary coalescing*: if the last base part and first tail part are both `text` (or both
   `reasoning`), concatenate their text — otherwise the answer renders as two markdown blocks
   split mid-sentence.

**Orphan tool-output holding.** A tail can carry a `tool-output-available` whose
`tool-input-*` frames predate the attach point. The AI-SDK reader treats that orphan as a
hard error and **silently ends the stream** — dropping everything after it, including the
final answer. Interpose a `TransformStream` (tail mode only): track `seenToolInputs` by
`toolCallId`; divert `tool-output-available`/`tool-output-error` chunks whose call id was
never seen into the `orphanOutputs` Map instead of enqueueing them.

**50ms flush throttle.** One `setMessages` per chunk re-renders the tree per chunk. Instead:
keep `pending` (latest reader message), schedule `flush()` via `setTimeout(flush, 50)` when
no timer is pending; `flush` does the upsert-or-splice in a single `setMessages` updater.
After the read loop, clear any timer and call `flush()` once unconditionally for final state.
Matches the primary transport's `experimental_throttle: 50`. Apply the hygiene fixes of
Known issue (g) while porting.

**Signature change.** `useSessionReconnect(threadId, status, setMessages, liveHeadSeq?)`.

### 3.4 Verification

- react-scan (§9): AgentCards/RunProgress 0 re-renders during a 10s pure-token window.
- Navigate away mid-swarm and back: repaint <300ms (upstream: 295ms), never blank, streams to
  completion, zero dup-key errors, task board once, final answer one contiguous markdown block.
- Kill the network mid-tail: what merged so far stays (the catch swallows the abort).

---

## 4. PR #12 (`8d85a5b`) — reasoning duration + metadata footer after reload

### 4.1 Intent

Live streams show "Thought for 4 seconds" and a `model · agent · duration` footer; after
reload both degraded (static "a few seconds", blank footer). Backend doc 07 persists reasoning
timing (`startedMs`/`endedMs`, unix-ms of first/last reasoning delta) and top-level
`model`/`agent_id`/`duration_ms` on archived messages; this PR rehydrates them, degrading
gracefully when the fields are absent (older docs).

### 4.2 Steps

1. **`lib/api/types.ts`** — `ThreadMessage` gains optional `model?: string`,
   `agent_id?: string`, `duration_ms?: number`.
2. **`lib/chat/from-history.ts`** — the `reasoning` case of `rehydratePart` preserves
   `startedMs`/`endedMs` when finite numbers (build as record, conditionally attach, cast to
   `Part`); a new `messageMetadata(m)` helper builds `{model?, agentId?, durationMs?}` from
   the top-level persisted fields (undefined when all absent), and `fromHistory` attaches it
   on BOTH branches — parts-rehydration AND text-fallback.
3. **`message-list.tsx`** reasoning case — derive the duration only when NOT live-streaming:

   ```tsx
   const reasoningDuration =
     !isReasoningStreaming &&
     typeof timing.startedMs === "number" && typeof timing.endedMs === "number" &&
     timing.endedMs >= timing.startedMs
       ? Math.max(1, Math.ceil((timing.endedMs - timing.startedMs) / 1000))
       : undefined;
   ```

   While streaming leave it `undefined` so the AI-elements `<Reasoning>` uses its own
   client-side timer.
4. **`message-reasoning.tsx`** — `MessageReasoning` gains optional `duration?: number`,
   forwarded to `<Reasoning duration={duration}>`.

The footer needs no component change — `message-meta.tsx` (doc 09) already reads
`message.metadata`; it was just never populated on reload.

### 4.3 Verification

Run a turn with visible reasoning; note the live "Thought for Ns". Reload: the collapse shows
the SAME real N (not "a few seconds"), the footer shows e.g.
`GPT-OSS 120B · swarm_coordinator · 14.6s`. A pre-migration thread (no persisted fields)
still renders the generic label, no footer, no crashes.

---

## 5. PR #13 (`ce88eca`) — Auto agent in the picker, default "auto"

### 5.1 Intent

Backend doc 07's classifier route accepts `agent_id: "auto"` and picks the best agent per
message. Frontend: an "Auto" item (Sparkles icon) atop the agent picker, `"auto"` as the
DEFAULT `selectedAgentId`, and a zustand persist migration so existing users actually see it.
The footer already shows the resolved agent via the `message-metadata` frame (PR #12).

### 5.2 Steps

1. **Name the sentinel** (00-README cross-cutting fix #3): `export const AUTO_AGENT_ID =
   "auto"` in one place (e.g. `stores/ui-store.ts`) and use it in the store default, the
   migrate, the selector, and the request body. Upstream scatters the literal — don't copy that.
2. **`agent-selector.tsx`** — `isAuto = selectedAgentId === AUTO_AGENT_ID`; trigger shows
   `Auto` with an accent `SparklesIcon` when auto, else the existing Bot icon/name;
   `active = isAuto || !!current` drives the highlighted style. Add the `CommandItem` at the
   top of the group (`value="auto"`, check mark bound to `isAuto`, subtitle "Automatically
   pick the best agent for each message"), above "No agent" — keep "No agent"
   (`setAgent(null)`) working.
3. **`stores/ui-store.ts`** — default `selectedAgentId: AUTO_AGENT_ID`; persist config gains
   `version: 1` and a `migrate` that force-resets `selectedAgentId` to `"auto"` while passing
   `selectedModel`/`reasoningEffort` through. Zustand only runs `migrate` when the persisted
   version is lower — without the bump existing users keep `null` and never see Auto.
   **Caution:** the hand-built migrate return must enumerate every `partialize` field and be
   extended whenever `partialize` grows (Known issue (f)). If your fork persists more prefs,
   carry them through explicitly.
4. **Request body gating** — Known issue (f): do NOT send `agentId: "auto"` for models
   without tool support. Upstream's `use-request-body.ts` sends `selectedAgentId ?? undefined`
   unconditionally; the picker hides for tool-less models but the store still says `"auto"`,
   so the backend receives a classifier request for a model that can't run agent tools. Gate
   in the body builder (fixed version in §7f).

### 5.3 Verification

Fresh profile: "Auto" selected. Existing v0 profile: after reload, "Auto" selected,
model/effort prefs intact. A research prompt routes to the researcher (footer shows it); "hi"
routes to the basic agent; manual agent and "No agent" still work. Tool-less model: picker
hidden AND the outgoing `POST /v1/chat/completions` body contains no `agent_id`.

---

## 6. PR #14 (`a4fefc1`) — question option rows + multi-question pager

### 6.1 Intent

Restyle only — the wire contract (`answers: string[][]` + optional joined `text`, POSTed as
`action: "approved"` to the resume endpoint) and the doc-09 behaviors (composer-swap, local
skip-without-deny, reopen, approve/deny) are unchanged. Pill chips become full-width
`OptionRow`s; N>1 questions page one-at-a-time, opencode-style. All in
`components/chat/messages/question-card.tsx` (~430 lines at main; read it whole).

### 6.2 Structure

- **`OptionRow`** — a `<button>` with a left radio dot (single) / checkbox glyph (multi),
  label + muted description, selected = `border-primary bg-primary/10 ring-1 ring-primary/40`;
  `role={multiple ? "checkbox" : "radio"}` + `aria-checked` (see Known issue (h) for the
  missing radiogroup wrapper). The "type your own" free-text row gets the same selected
  styling when its textarea has content.
- **Settled early-return.** Settled/inline-history rendering becomes a dedicated early-return
  BEFORE the live form: read-only recap of ALL questions with chosen answers as check-badges,
  "No answer" placeholders, and the joined `answerText`. Paging exists only while answering.
- **Pager state.** `activeTab`; `pager = !settled && total > 1`; `shown = pager ? [activeTab]
  : all`. Header carries the progress-dot row — one small bar per question, `data-active`
  (solid primary) / `data-answered` (`isAnswered(qi)` = selection or non-empty text →
  primary/50), clickable to jump — plus a `N of Total` counter. Footer:
  `Dismiss | (Back when activeTab>0) | Next` on non-last tabs, `Dismiss | (Back) | Submit`
  (disabled unless `hasAnswer`) on the last. Single question: no pager, `Dismiss | Submit`.
- **Cmd/Ctrl+Enter** advances: `advance()` = next tab, or `submit()` on the last/only page.
  Wired on BOTH the card container's `onKeyDown` and the free-text `Textarea`'s `onKeyDown` —
  **this double wiring is the critical defect; implement §7a's fixed version, not upstream's.**

### 6.3 Verification

Single question: radio rows, no pager, submit resolves 200. Three-in-one: 3-dot pager,
answers accumulate across tabs, Submit posts the full matrix
(`answers: [["Red"],["Summer"],["Pizza"]]`). Multi-select shows checkboxes. Dismiss keeps the
run `awaiting-input` and reopenable. **One** `POST /v1/agent/resume` per submit — watch the
network tab while submitting via Cmd+Enter focused in the textarea (§7a).

---

## 7. Known issues — fix during port

These defects exist in the reference repo at `a5c544e`; the fork lands the FIXED versions.

### (a) CRITICAL — PR #14: Cmd/Ctrl+Enter in the textarea double-fires `advance()`

Upstream wires the shortcut on the card `<div onKeyDown>` AND on the `Textarea onKeyDown`.
The textarea handler calls `e.preventDefault()` but **not `e.stopPropagation()`**, so the
event bubbles to the card handler and `advance()` runs twice. On the last tab `advance()` is
`void submit()` — and the `submitting` **state** guard passes on the second same-tick call
(setState hasn't re-rendered), so the backend gets a **DOUBLE interrupt-resolve POST** for
one toolCallId. Two fixes, both required:

```tsx
// 1. textarea handler must stop propagation
onKeyDown={(e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
    e.preventDefault();
    e.stopPropagation();   // card-level handler also listens — without this, double-fire
    advance();
  }
}}
// 2. same-tick reentrancy guard must be a ref, not state
const submittingRef = useRef(false);
const submit = async () => {
  if (!resolveInterrupt || submittingRef.current) return;
  submittingRef.current = true;
  setSubmitting(true);              // keep the state for disabled styling only
  try { /* ... resolveInterrupt ... */ }
  finally { submittingRef.current = false; setSubmitting(false); }
};
```

### (b) PR #11: `artifactSig` must be content-bearing, not a count

Upstream keys the `collectArtifacts` memo on the **count** of `data-artifact` parts. But
artifact parts **upsert in-place by id** — an UPDATE (new content, same id) changes no count,
the memo never recomputes, and the artifact panel **freezes on updates**. Include
content-bearing fields:

```tsx
let artifactSig = "";
for (const m of messages)
  for (const p of m.parts)
    if (p.type === "data-artifact") artifactSig += `${p.data.id}:${p.data.title ?? ""};`;
const artifacts = useMemo(() => collectArtifacts(messages), [artifactSig]);
```

If your fork's artifact updates can change other panel-rendered fields (e.g. a version
counter), fold those in too — §3.1's principle: *signature over exactly the rendered props.*

### (c) PR #11: `agentCardsKey`'s progress segment must include the agent name

Upstream (current main, `agent-cards.tsx:29`):

```ts
else if (p.type === "data-agent-progress" && p.data.agent)
  prog += `${p.data.message};`;          // BUG: drops p.data.agent
```

Two agents interleaving identical step messages (`"searching"` from A-then-B vs. B-then-A)
alias to the SAME signature — the comparator wrongly bails and a card's step list goes stale.
Fixed:

```ts
prog += `${p.data.agent}:${p.data.message};`;
```

### (d) PR #11: `runProgressKey` should compare `message.metadata?.agentId`

`RunProgress` renders the agent label from `message.metadata?.agentId`, but the comparator
only hashes progress-part messages. The `message-metadata` frame typically lands AFTER the
first progress parts — the comparator blocks that re-render, so the header shows the wrong/no
agent until the next progress event. Fix in the comparator:

```ts
(a, b) =>
  a.streaming === b.streaming &&
  a.isLast === b.isLast &&
  a.stopped === b.stopped &&
  a.message.metadata?.agentId === b.message.metadata?.agentId &&
  runProgressKey(a.message) === runProgressKey(b.message)
```

### (e) PR #10: `start_seq` / `turn` must be OPTIONAL on `SessionHandle`

Upstream declares them required (`start_seq: number; turn: number`) but the reconnect code
already defends (`(startSeq ?? 1) - 1`). A backend mid-upgrade, or a handle cached before the
upgrade, omits them — a required type hides that from every consumer. Declare:

```ts
start_seq?: number;  // absent ⇒ reconnect falls back to after=0 full replay (idempotent via ids)
turn?: number;
```

Document the fallback at the use site: `after = Math.max(0, (startSeq ?? 1) - 1)` degrades to
`after=0` when absent — safe (deterministic ids), just not cheap.

### (f) PR #13: gate `agentId: "auto"` on tool support; name the constant; extend migrate with partialize

1. Upstream's `use-request-body.ts` sends `agentId: selectedAgentId ?? undefined`
   unconditionally. With the `"auto"` default, tool-less models (whose picker is hidden)
   still send `agent_id: "auto"`. Fixed body builder:

   ```ts
   const toolsOk = modelSupportsTools(useSelectedModel());
   // ...
   agentId: toolsOk ? (selectedAgentId ?? undefined) : undefined,
   ```

2. `export const AUTO_AGENT_ID = "auto"` once; use it everywhere the sentinel appears
   (00-README cross-cutting fix #3).
3. The persist `migrate` hand-builds its return object; comment `partialize` that any field
   added there MUST be carried through `migrate`. Safer still:
   `migrate: (s) => ({ ...(s as object), selectedAgentId: AUTO_AGENT_ID })` — spread-through
   beats enumeration; verify against your fork's partialize shape.

### (g) PR #11: reconnect hook hygiene

Three fixes in `use-session-reconnect.ts`:

1. **Clear the flush timer in the effect cleanup.** Upstream clears it only in the async
   IIFE's `finally`; the effect cleanup just calls `controller.abort()`, so a `flush`
   scheduled right before unmount can fire `setMessages` after unmount. Hoist `timer` to
   effect scope and:

   ```ts
   return () => {
     controller.abort();
     if (timer != null) clearTimeout(timer);
   };
   ```

2. **Don't mutate `baseParts` inside the `setMessages` updater.** Upstream does
   `baseParts.set(msg.id, prev[idx].parts)` inside the updater — a side effect in what React
   treats as pure; under StrictMode double-invocation the captured base can differ between
   invocations. Preferred: capture each id's base OUTSIDE the updater, on first sight of the
   id. If you keep the in-updater write it must be set-once-when-absent and never
   overwritten — comment the invariant and verify under StrictMode.

3. **Move ref writes out of render.** `if (primaryBusy) primaryOwnedRef.current = true;`
   runs during render; move it into
   `useEffect(() => { if (primaryBusy) primaryOwnedRef.current = true; }, [primaryBusy])`.
   (`headSeqRef` as a pure `useRef(initial)` is fine.)

### (h) PR #14: radiogroup a11y (defer OK)

`OptionRow` buttons carry `role="radio"` but the list has no `role="radiogroup"`, no
`aria-labelledby` pointing at the question text, and no roving tabindex — screen readers
announce orphaned radios. Minimal fix: wrap the rows in
`<div role="radiogroup" aria-labelledby={questionHeadingId}>` (`role="group"` for
multi-select) and give the question `<p>` that id; arrow-key roving tabindex is the full fix.
May be deferred to a follow-up if time-boxed; items (a)–(g) may not.

---

## 8. Fork-adaptation notes

- **File map (post-refactor main):** `components/chat/use-session-reconnect.ts`,
  `use-agent-chat.ts`, `use-request-body.ts`, `thread-view.tsx`,
  `messages/{agent-cards,run-progress,message-list,message-reasoning,question-card}.tsx`,
  `composer/{agent-selector,thread-usage-ring}.tsx`, `lib/api/{sessions,threads,types}.ts`,
  `lib/chat/{from-history,sse-to-chunks}.ts`, `stores/ui-store.ts`,
  `components/sidebar/app-sidebar.tsx`. Fork paths may differ — port mechanisms, not paths.
- **If your fork followed doc 09's SKIPs**, PR #9's agent-card half is a no-op (no
  `AgentLivePreview` to delete), and there is no separate "Running sessions" sidebar section
  to reconcile.
- **AI SDK version sensitivity:** `readUIMessageStream`, `experimental_throttle`, the
  `UIMessageChunk` union (`tool-input-start/available`, `tool-output-available/error`) and
  `onFinish({messages})` are AI SDK v5 shapes. If the fork pins a different minor, verify the
  chunk type names in the orphan-holding TransformStream against your version's union — a
  renamed chunk type silently defeats the filter and resurrects the "stream silently ends" bug.
- **Signature discipline is the exported lesson.** Any new rendered field added to
  AgentCards/RunProgress/the artifact list MUST also be added to the corresponding key
  function — that is exactly how upstream broke (c)/(d). Comment each key function:
  "renders X, Y, Z ⇒ signature hashes X, Y, Z — keep in lockstep."
- **Order within this phase:** #9 → #10 → #11 → #12 → #13 → #14 (one branch/PR each, per
  00-README rule 3). #11 depends on #10's `[DONE]` scoping and SessionHandle fields; #12–#14
  are mutually independent after #11.
- **Zustand migrate testing:** before shipping #13, seed `localStorage["agentui-ui"]` with a
  v0 payload (your fork's real pre-bump shape) and assert the migrated store. If the fork
  already bumped persist `version`, bump to fork-version+1 and merge the migrations.

## 9. Verification — phase exit checklist

`npx tsc --noEmit` and eslint clean after every PR. Then:

**FPS methodology (react-scan).** Dev only: `npx react-scan@latest http://localhost:3000`
against the running dev server (or the `<Monitoring>` snippet). Steps:

1. Idle thread open, no stream: baseline ~120fps (your display's refresh), zero highlighted
   re-renders.
2. Start a swarm run (a prompt fanning out to 3+ sub-agents). During a 10s window of pure
   token streaming (no lifecycle events): `AgentCards` and `RunProgress` show **0** re-renders
   (upstream: 175→0 each); FPS stays at/near baseline (upstream: ~1 → ~120). The message list
   itself still repaints on the 50ms throttle — expected; the O(parts) scanners must not.
3. Confirm the cards still DO update on real events: a sub-agent finishing flips its badge
   within one flush.

**Resume.** Navigate away mid-swarm, return: repaint **<300ms**, **never blank**, tail keeps
streaming, task board appears once, completion is one contiguous answer. Repeat with a cold
reload. Then send a follow-up turn: zero dup-key console errors, no corruption of the
previous turn (the `[DONE]` scoping test).

**Metadata.** Reload a finished reasoning thread: real "Thought for Ns" + populated
`model · agent · duration` footer (§4.3).

**Auto agent.** §5.3 checklist, including the no-`agent_id`-for-tool-less-models network
assertion.

**Question pager.** §6.3 checklist, plus the (a)-fix regression: Cmd+Enter focused in the
free-text textarea on the LAST tab → exactly one resume POST; on a non-last tab → the pager
advances exactly one step.

**Known-issue spot checks:** artifact update (same id, new content) repaints the panel (b);
interleaved sub-agents with identical step text show correct per-card step lists (c);
RunProgress header shows the routed agent as soon as the metadata frame lands (d).
