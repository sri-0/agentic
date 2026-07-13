# 09 — Frontend: Swarm Streaming, Question Composer, Session Status, File Artifacts, Integrated Sidebar

> **Series**: agentui frontend migration (Next.js/React, merged main @ `a5c544e`).
> **Prerequisite**: doc `08` (frontend baseline: AI SDK v6 chat wiring,
> `ChatDataParts` plumbing, side panel, sessions API client).
> **Audience**: an agent porting into a **diverged fork**. Do not blind-copy —
> read each section's intent and key code, then implement the equivalent inside
> the fork's existing structure, preserving fork-local changes.

Source repo for all quoted code: `/Users/sri/code/agentui` (branch `main`).
Covers agentui PRs **#4–#8** (commits `75da240`, `59c4b98`, `6d6891f`,
`978ae6e`, `69e0dc2`); where later PRs (#9–#15) amended a feature, this doc
describes the **durable, current-main design** and flags what NOT to build.

---

## 1. Purpose

Five user-facing gaps closed in one round:

| PR | Feature | One-liner |
|----|---------|-----------|
| #4 | Swarm streaming | Sub-agent activity visible live: spinners on the task board, clickable task rows opening the child's stream in the side panel, orchestration tool-cards hidden |
| #5 | Question composer swap | A pending `question`-tool interrupt REPLACES the chat composer (ChatGPT-style); Dismiss = local skip, reopenable |
| #6 | Session status | Sidebar thread rows show live run status joined from `/v1/sessions`; mark-viewed on open; focus refetch for late tool parts |
| #7 | File artifacts | `kind:"file"` artifacts (office docs) render as a download card, not a raw URL |
| #8 | Integrated sidebar | The separate "Running" sessions section is DELETED; a single 4-state icon lives on each thread row |

Files touched (current-main state — study these, not just the diffs):
`lib/chat/tasks.ts` (**keystone**, quoted in full §3.1); `lib/chat/types.ts`
(task-list `running` status, artifact `file` kind); `components/chat/task-bar.tsx`;
`components/chat/thread-view.tsx` (`ThreadChat` — `activeQuestion`, composer swap);
`components/chat/messages/{message-list,question-card,agent-cards}.tsx`;
`stores/ui-store.ts` (`skippedQuestions`); `components/sidebar/app-sidebar.tsx`
(`sessionByThread`, 4-state icon); `lib/api/sessions.ts` (`viewed`, `useMarkViewed`);
`lib/api/threads.ts` (`refetchOnWindowFocus`);
`components/right-panel/artifact-view.tsx` (`FileCard`); `lib/chat/artifacts.ts`
(`downloadArtifact` remote branch).

---

## 2. The part-type contract these features consume

Custom data parts from the Go backend's AI SDK v6 stream: each `ChatDataParts`
key becomes a `data-<key>` part type in `message.parts`. The fork MUST have
these exact shapes (from `lib/chat/types.ts`) before starting:

```ts
export type ChatDataParts = {
  /** sub-agent lifecycle (started/done). durationMs set on the `done` step. */
  "agent-step": {
    agent: string;                  // ⚠ keyed "<subagentType>#<short>" for swarm children
    step: number;
    status: "started" | "done";
    durationMs?: number;
  };
  /** incremental token chunk of a sub-agent's own output; client concatenates
   *  a given agent's deltas (per kind) to reconstruct its output. */
  "agent-delta": {
    agent: string;                  // ⚠ same "<type>#<short>" key
    step: number;
    kind: "reasoning" | "text";
    delta: string;
  };
  /** transient status line ("Analyzing…") — drives the card step timeline */
  "agent-progress": { phase: string; message: string; agent?: string };
  /** todo/task snapshot; REPLACES the previous board wholesale */
  "task-list": {
    tasks: {
      id: string;                   // swarm child: "<parentID>:<subagentType>-<short>"
      title: string;
      status: "pending" | "in_progress" | "running" | "completed" | "cancelled";
      priority?: "high" | "medium" | "low";
      agent?: string;               // ⚠ BARE "<subagentType>" — NOT the stream key
    }[];
  };
  artifact: {
    id: string;                     // re-emit same id to update
    title: string;
    kind: "markdown" | "code" | "html" | "json" | "csv" | "file";
    content: string;                // empty for kind:"file"
    language?: string;
    url?: string;                   // file-kind: remote binary, Content-Disposition: attachment
    mime?: string;
    filename?: string;
    size?: number;
  };
  // ... tool-interrupt (questions), usage — see doc 08
};
```

**The key mismatch that PR #4 exists to solve** (memorize this):

- Streamed parts (`data-agent-delta` / `data-agent-step` / `data-agent-progress`)
  carry `agent = "<subagentType>#<short>"` (e.g. `researcher#a1b2c3d4`), where
  `<short>` is an 8-char uuid prefix of the child session.
- The `data-task-list` rows carry `agent = "<subagentType>"` (bare) and
  `id = "<parentID>:<subagentType>-<short>"`.
- The side panel binds to the STREAM key. Binding it to the task row's bare
  `agent` matches zero parts → empty panel. The shared `<short>` bridges the two.

Also: the backend emits `status:"running"` for in-progress swarm children
(task.go), while `todowrite` emits `"in_progress"` — treat both as one state.

---

## 3. PR #4 — Swarm streaming (`75da240`)

### 3.1 Keystone: `lib/chat/tasks.ts` (quote — port this module essentially verbatim)

Small, pure, type-only imports; encodes both contract quirks above:

```ts
import type { ChatDataParts, ChatMessage } from "@/lib/chat/types";

export type TaskItem = ChatDataParts["task-list"]["tasks"][number];
export type TaskStatus = TaskItem["status"];

/** Normalized lifecycle state for a swarm task/sub-agent. The backend emits
 *  `"running"` for in-progress children (task.go) while the todowrite tool uses
 *  `"in_progress"` — collapse both to a single "active" state so the UI shows the
 *  same loading/spinner treatment regardless of source. */
export type TaskPhase = "pending" | "active" | "done" | "cancelled";

export function normalizeTaskStatus(status: TaskStatus): TaskPhase {
  switch (status) {
    case "in_progress":
    case "running":
      return "active";
    case "completed":
      return "done";
    case "cancelled":
      return "cancelled";
    default:
      return "pending";
  }
}

/** True while a task is still being worked (spinner state). */
export function isTaskActive(status: TaskStatus): boolean {
  return normalizeTaskStatus(status) === "active";
}

/** True once a task has reached a terminal state (completed or cancelled). */
export function isTaskSettled(status: TaskStatus): boolean {
  const p = normalizeTaskStatus(status);
  return p === "done" || p === "cancelled";
}

/**
 * Resolve the sub-agent STREAM KEY (the `agent` label carried on
 * `data-agent-delta`/`data-agent-step` parts) for a swarm task row.
 *
 * Root cause of "clicking a task row opened an empty panel": the task list
 * (task.go) records `id = "<parentID>:<subagentType>-<short>"` and
 * `agent = "<subagentType>"` (the bare type), but every streamed part is keyed
 * by `label = "<subagentType>#<short>"`. The bare type never matches a delta's
 * agent, so the side panel bound to `t.agent` saw no parts.
 *
 * The `<short>` is a stable 8-char uuid prefix shared by both forms, so we
 * prefer to find the live delta/step agent whose `#<short>` suffix matches the
 * task id. Falling back to reconstructing `"<type>#<short>"` from the id keeps
 * the row clickable even before the first delta arrives.
 */
export function taskAgentKey(task: TaskItem, messages: ChatMessage[]): string {
  const short = childShort(task.id);
  if (short) {
    for (let i = messages.length - 1; i >= 0; i--) {
      for (const p of messages[i].parts) {
        if (
          (p.type === "data-agent-delta" ||
            p.type === "data-agent-step" ||
            p.type === "data-agent-progress") &&
          typeof p.data.agent === "string" &&
          p.data.agent.endsWith(`#${short}`)
        ) {
          return p.data.agent;
        }
      }
    }
    // No live part yet — reconstruct the label from the id + bare type.
    if (task.agent) return `${task.agent}#${short}`;
  }
  return task.agent ?? task.id;
}

/** The trailing 8-char short id shared by a child's session id and stream label,
 *  or "" if the id doesn't carry one. */
function childShort(id: string): string {
  const seg = id.slice(id.lastIndexOf(":") + 1); // "<type>-<short>"
  const dash = seg.lastIndexOf("-");
  if (dash === -1) return "";
  const short = seg.slice(dash + 1);
  return short.length >= 4 ? short : "";
}
```

### 3.2 Type change

In the fork's `ChatDataParts["task-list"]` status union, add `"running"`
alongside `"in_progress"` (see §2). Without it, TS rejects `normalizeTaskStatus`
and — pre-fix — in-progress swarm children rendered as idle/pending circles.

### 3.3 TaskBar rework (`components/chat/task-bar.tsx`)

Intent: (a) spinner on active children via `isTaskActive`; (b) board stays
visible until the **coordinator run** settles, not merely when child counts
match; (c) task rows are buttons opening the child's stream in the side panel;
(d) per-token re-renders skipped via a content-signature memo.

Key mechanics to reproduce (adapt markup to the fork's TaskBar):

1. `latestTasks(messages)` — scan messages backwards for the newest
   `data-task-list` part; return `{ tasks, messageId }`. Guard
   `Array.isArray(part.data.tasks)` (tasks can arrive null). `messageId` matters:
   the side panel needs the owning message to find the child's parts.
2. TaskBar takes a `running: boolean` prop —
   `status === "streaming" || status === "submitted"` from the chat hook, passed
   by `ThreadChat`. Hide condition:
   ```ts
   const done = tasks.filter((t) => isTaskSettled(t.status)).length;
   if (!running && done === tasks.length) return null;
   ```
   Previously the bar vanished the moment `done === tasks.length` even though
   the coordinator was still synthesizing the final answer.
3. Memo comparator on a **signature string** with `running` folded in:
   ```ts
   function taskSignature(messages: ChatMessage[], running: boolean): string {
     const latest = latestTasks(messages);
     if (!latest) return `|${running}`;
     let s = "";
     for (const t of latest.tasks) s += `${t.id}:${t.status}:${t.agent ?? ""};`;
     return `${s}|${running}`;
   }
   // memo(TaskBar, (a, b) => taskSignature(a.messages, a.running) === taskSignature(b.messages, b.running))
   ```
   Folding `running` in makes the appear/disappear transitions re-render
   through the memo. Replace any message-array-identity comparator in the fork.
4. Row click:
   ```tsx
   onClick={() => clickable && openSidepanel({
     kind: "agent",
     agent: taskAgentKey(t, messages),   // ← NEVER t.agent directly
     messageId,
   })}
   ```
   `clickable = Boolean(t.agent)`. Row icons: cancelled → XCircle, completed →
   green check, `isTaskActive` → spinning loader, else hollow circle.

### 3.4 Hide orchestration tool cards (`message-list.tsx`)

```ts
const HIDDEN_TOOL_CARDS = new Set(["task", "task_join", "todowrite"]);
```

In the message-list part-classification pass, `dynamic-tool` parts whose
`toolName` is in this set are skipped (an early `break`/`continue`, mirroring
the existing `question` special-case). Safe because the task board is driven by
the separate `data-task-list` part, not these tool calls. Apply the same name
filter at whatever choke point the fork classifies tool parts.

### 3.5 ⚠ Do NOT build `AgentLivePreview`

PR #4 also added an inline `AgentLivePreview` inside `AgentCards` — a Streamdown
tail-clip of each working child's streamed text. **PR #9 (doc 10) removed it for
performance** ("drop glitchy agent-card text"): a markdown re-parse per child
per token batch was too hot for swarm turns, and the clipped tail glitched.

The **durable design the fork should implement directly**:

- `AgentCards` shows an identity header (`type` + `#short` instance suffix), a
  status badge (working spinner / done + `durationMs` / error), and a **step
  timeline built from `data-agent-progress` parts** — consecutive duplicates
  collapsed into `×count`, latest non-error step "active" while working. No
  streamed text inline.
- Full streaming text (reasoning + answer, concatenated from `data-agent-delta`)
  lives in the **side panel only**, opened by clicking a card or task row.
- Re-render gating: content-signature memo (`agentCardsKey`) from agents-seen +
  lifecycle + progress — **excluding delta text** — plus `useMemo` over the
  `collectAgents` scan keyed on the same signature. Only agents with at least
  one `data-agent-delta` get a card (the top-level/output agent streams into
  the main thread; no double-carding).

If the fork suppressed `AgentCards` for swarm runs behind a `!isSwarm` gate,
remove the gate — swarm children should get cards.

### 3.6 Verification (#4)

`tsc --noEmit` + eslint clean after the type-union change; then the §9 swarm
items (spinners, board persistence, row-click → live panel, no raw cards).

---

## 4. PR #5 — Question composer swap (`59c4b98`)

Intent: ChatGPT-style — while a `question`-tool interrupt is pending, the chat
composer is replaced by an interactive question panel in the footer slot.
"Dismiss" must NOT consume the run (the old code POSTed `action:"denied"`,
killing it); it skips locally and stays reopenable.

### 4.1 Local skip state (`stores/ui-store.ts`)

Add to the UI store:

```ts
/** Client-only set of question-interrupt toolCallIds the user has SKIPPED. */
skippedQuestions: Set<string>;
skipQuestion: (toolCallId: string) => void;    // add to a COPIED Set (immutable update)
unskipQuestion: (toolCallId: string) => void;  // delete from a copied Set
```

Both actions no-op (return same state) if membership already matches.
**Critical**: `skippedQuestions` is EXCLUDED from the zustand `persist`
`partialize` (which keeps only model/agent/effort prefs). Two reasons: a `Set`
does not survive JSON serialization (rehydrates as `{}`, `.has()` throws), and
the skip is deliberately ephemeral — the run stays `awaiting-input` server-side,
so a reload legitimately re-surfaces the question. If the fork's store persists
everything, add/extend `partialize`.

### 4.2 `activeQuestion` derivation (`ThreadChat` in `components/chat/thread-view.tsx`)

```ts
const skippedQuestions = useUiStore((s) => s.skippedQuestions);

// Newest UNRESOLVED, NON-SKIPPED question interrupt across the thread.
const activeQuestion = useMemo(() => {
  for (let i = messages.length - 1; i >= 0; i--) {
    const parts = messages[i].parts;
    for (let j = parts.length - 1; j >= 0; j--) {
      const p = parts[j];
      if (
        p.type === "data-tool-interrupt" &&
        isQuestionInterrupt(p.data) &&
        !p.data.resolved &&
        !skippedQuestions.has(p.data.toolCallId)
      ) {
        return p.data;
      }
    }
  }
  return null;
}, [messages, skippedQuestions]);
```

`isQuestionInterrupt` (exported from `question-card.tsx`) is
`toolName === "question" || extractQuestions(interrupt) != null`, where
`extractQuestions` accepts `data.questions` OR `data.details.questions`.
Approve/deny interrupts (e.g. `write_database`) return false → never swap the
composer; they keep their inline ToolCard.

Footer slot swap: `{activeQuestion ? <QuestionCard key={activeQuestion.toolCallId}
interrupt={activeQuestion} variant="composer" /> : <Composer ... />}` — the
`key` on toolCallId forces fresh form state per question.

### 4.3 QuestionCard changes (`components/chat/messages/question-card.tsx`)

- New prop `variant?: "inline" | "composer"` (default inline). Composer variant
  only changes styling (floating-dock card classes) — same form logic.
- `skip()` (Dismiss) becomes `() => skipQuestion(interrupt.toolCallId)`. It
  previously called `resolveInterrupt(toolCallId, "denied")` — **remove that
  POST**; no network call on Dismiss.
- Submit is unchanged: `resolveInterrupt(toolCallId, "approved", { answers, text })`
  (answers = `string[][]` of selected labels, free text folded in).
- Include `a.variant === b.variant` in the memo comparator.

### 4.4 No-double-render + reopen button (`message-list.tsx`)

`MessageList` (and `MessageItem`) take `activeQuestionId?: string`
(= `activeQuestion?.toolCallId` from `ThreadChat`). In the per-message part
classification for question interrupts:

- `resolved` → render inline (read-only history recap).
- `toolCallId === activeQuestionId` → render NOTHING inline (it lives in the
  composer slot).
- in `skippedQuestions` → collect the id into a local `skippedIds` list.
- otherwise (unresolved, not-yet-active — reconnect edge) → render inline.

If `skippedIds.length > 0`, render a "Skipped Questions" button on that message
calling `skippedIds.forEach(unskipQuestion)` — `activeQuestion` re-derives and
the panel returns to the composer slot.

### 4.5 Verification (#5)

§9 question items, plus: an approve/deny interrupt does NOT swap the composer,
and a reload while a question is skipped re-surfaces it (skip did not persist).

---

## 5. PR #6 — Session status plumbing (`6d6891f`)

Intent: sidebar rows reflect live run state; opening a finished thread clears
its "unread" marker exactly once; tool cards appear reliably on threads opened
as a run finishes. (#6 is the data layer; PR #8, §7, is the presentation —
port them together.)

### 5.1 API layer (`lib/api/sessions.ts`)

- `SessionHandle` gains `viewed: boolean` (backend defaults false for terminal
  runs; active runs are effectively not-unviewed).
- `useMarkViewed()` mutation: `POST /v1/sessions/{id}/viewed`; swallow 404
  (`ApiError`) — not-owner/no-session is non-actionable; `onSuccess` invalidate
  `["sessions"]` so the ring clears immediately instead of waiting for the 5s
  poll. `useSessions()` keeps `refetchInterval: 5000`.

### 5.2 Join map (`app-sidebar.tsx`)

Sessions are keyed `session_id === thread.id` (persisted threads own one server
session under their own id). Build the join ONCE per sessions change:

```ts
const sessionByThread = useMemo(
  () => new Map(sessions.map((s) => [s.session_id, s] as const)),
  [sessions],
); // per row: session={sessionByThread.get(t.id)}
```

Not `sessions.find()` per row — the 5s poll would fan out into a per-row
re-render storm on large thread lists.

### 5.3 Mark-viewed latch (`ThreadRow`)

```ts
const activeSession = session && isActiveStatus(session.status); // running|awaiting-input|queued
const terminal = session && !activeSession;
const unviewed = Boolean(terminal && !session.viewed);
const shouldMarkViewed = Boolean(active && unviewed);   // `active` = row is the open thread
const viewedRef = useRef(false);
useEffect(() => {
  if (!active) { viewedRef.current = false; return; }   // reset → later reopen re-marks
  if (shouldMarkViewed && !viewedRef.current) {
    viewedRef.current = true;
    mutate(id);
  }
}, [active, shouldMarkViewed, id, mutate]);
```

The latch covers the window between the POST and the `["sessions"]` refetch
(where `unviewed` is still true → the effect would re-POST); resetting on
`!active` lets a later NEW terminal run on the same thread re-mark.

### 5.4 `useThreadMessages` robustness (`lib/api/threads.ts`)

Add `refetchOnWindowFocus: true` to the thread-messages query — returning to a
just-finished thread re-pulls history so late-persisted tool parts appear.

**⚠ SKIP the one-shot 1.2s `setTimeout` invalidate.** Current main also carries
a `useEffect` invalidating `["thread-messages", threadId]` 1.2s after open.
Backend #17 made part-persistence flush synchronously before the run reports
done, so the timer is dead-weight (an extra fetch per thread open for a race
that no longer exists). Do not port it; `refetchOnWindowFocus` is the durable piece.

### 5.5 Verification (#6)

Running → done within one 5s poll tick; exactly ONE viewed POST per open
(Network tab); blur/refocus → one history refetch. See §9.

---

## 6. PR #7 — File artifacts (`978ae6e`)

Intent: the backend emits `data-artifact` `kind:"file"` + `url/mime/filename/size`
for generated office docs (pptx/docx/xlsx), served with
`Content-Disposition: attachment`. Render a download card, not a raw URL.

### 6.1 Types (`lib/chat/types.ts`)

Add `"file"` to the artifact `kind` union + optional `url`/`mime`/`filename`/`size`
(see §2); `content` stays required (empty for files). History rehydration
(`from-history.ts` equivalent) needs no change **if** it passes artifact `data`
through opaquely — if the fork whitelists fields, add the four new ones.

### 6.2 `downloadArtifact` (`lib/chat/artifacts.ts`)

Add a remote branch ahead of the existing Blob path:

```ts
export function downloadArtifact(a: ArtifactData) {
  if (a.kind === "file") {           // ⚠ see fix note below
    if (!a.url) return;
    const el = document.createElement("a");
    el.href = a.url;
    if (a.filename) el.download = a.filename;
    el.rel = "noopener";
    document.body.appendChild(el);
    el.click();
    el.remove();
    return;
  }
  // ...existing Blob path for text kinds (csv/json/html/markdown/code)
}
```

**Known issue — fix while porting**: main's condition is
`if (a.kind === "file" || a.url)`, widening the remote branch to ANY kind
carrying a `url` — a text artifact with a url would skip the Blob path and
download the remote resource instead of its `content`. Narrow to
`a.kind === "file"` only, as shown.

### 6.3 `FileCard` (`components/right-panel/artifact-view.tsx`)

In the panel's `ArtifactBody`, branch FIRST on `kind === "file"` →
`<FileCard>` (binaries must never fall through to the Streamdown/markdown
renderer). Card contents:

- File icon tile + `filename ?? title`.
- Friendly type label from extension or mime: `.pptx`/`presentation` →
  "PowerPoint presentation", `.docx`/`wordprocessing` → "Word document",
  `.xlsx`/`spreadsheet` → "Excel spreadsheet", else raw mime or "File".
- `formatSize(bytes)` (B/KB/MB) when `size != null`.
- "Download" → `downloadArtifact(artifact)`, `disabled={!artifact.url}`; plus an
  "Open in new tab" anchor (`target="_blank" rel="noopener noreferrer"`) when
  `url` is present.

### 6.4 Inline transcript card (`message-list.tsx`)

The inline `data-artifact` chip picks its icon per kind:
`part.data.kind === "file" ? <FileIcon/> : <FileTextIcon/>` (still opens the
side panel on click, same as other kinds).

### 6.5 Verification (#7)

Office-doc items in §9; regression-check that a csv/markdown artifact still
downloads via the Blob path after the §6.2 narrowing.

---

## 7. PR #8 — Integrated sidebar (`69e0dc2`)

Intent (explicit user decision): **no separate agent-sessions sidebar
section** — status lives on the thread rows themselves.

### 7.1 Delete

Remove (if present from an earlier port): the `RunningSessions` group,
`SessionRow`, and the `STATUS_STYLE` map. The `sessionByThread` join,
`useMarkViewed`, and the §5 latch all stay.

### 7.2 4-state icon on thread rows

One trailing icon per row, priority-ordered:

| Priority | Condition | Icon |
|---|---|---|
| 1 | `status === "awaiting-input"` | amber `CircleAlert` (needs your input) |
| 2 | `running` / `queued` (`isActiveStatus`) | green pulse — `animate-ping` ring + solid emerald core (`StatusDot`) |
| 3 | terminal `&& !viewed` | blue hollow ring (`CompletedRing viewed={false}`) — unread result |
| 4 | terminal `&& viewed` | gray hollow ring — seen |
| — | no joined session | no icon |

`StatusDot` = absolute `animate-ping` emerald ring over a solid emerald core
(size-2); `CompletedRing` = `size-2.5 rounded-full border-2`, border blue-500
when unviewed, muted gray when viewed. Adapt classes to the fork's tokens.

Render inside the row — note the awaiting-input check comes BEFORE the generic
active check (awaiting-input is also "active" per `isActiveStatus`):

```tsx
{session && (
  <span className="pointer-events-none absolute right-1 top-1/2 -translate-y-1/2
                   transition-opacity group-hover/menu-item:opacity-0">
    {session.status === "awaiting-input" ? (
      <CircleAlertIcon className="size-3.5 text-amber-500" />
    ) : activeSession ? (
      <StatusDot />
    ) : (
      <CompletedRing viewed={!unviewed} />
    )}
  </span>
)}
```

### 7.3 Trash hover overlays the icon slot

The icon sits in the SAME right-edge slot as the hover-delete action
(`SidebarMenuAction showOnHover`) and fades on row hover
(`group-hover/menu-item:opacity-0`, `pointer-events-none`) so the trash replaces
it with zero layout shift. Non-shadcn forks: status icon and delete button share
one absolutely-positioned slot; hover swaps their opacity.

### 7.4 Verification (#8)

All four states visible + no separate Running section + delete still works on
hover (icon fades, trash clickable). See §9 for the scripted walk-through.

---

## 8. Fork-adaptation notes

1. **Implement, don't overwrite.** Diff each fork file against the quoted
   intent first; fork-local styling/metadata stay. The only near-verbatim copy
   is `lib/chat/tasks.ts` (§3.1).
2. **Order**: #4 → #5 are independent of #6 → #8; #7 is independent of all.
   Land #6 (data layer) and #8 (presentation) as ONE change — do not build #6's
   original separate Running section only to delete it.
3. **Do not build `AgentLivePreview`** (§3.5) even though it is in the #4 diff:
   cards = steps/progress timeline; full streamed text = side panel only.
4. **Skip the 1.2s thread-messages invalidate** (§5.4); keep `refetchOnWindowFocus`.
5. **Narrow `downloadArtifact`** to `a.kind === "file"` (§6.2).
6. If the fork's side panel binds agent streams by a different key, adapt
   `taskAgentKey` to return YOUR key — the invariant is "task row → the key
   under which that child's deltas are stored", bridged via `#<short>`.
7. Naming drift: here the chat page component is `ThreadChat` in
   `components/chat/thread-view.tsx`, and `components/right-panel/artifact-view.tsx`
   is what other docs may call `components/artifacts/artifact-view.tsx`.
8. Memo discipline matters more than markup: every component scanning
   `message.parts` (TaskBar, AgentCards) needs a content-signature comparator,
   or swarm turns (thousands of delta parts) melt the frame budget.

### Known issues to carry as comments (not fix-blockers)

- **`skippedQuestions` is never pruned** — ids accumulate for the session
  lifetime (not persisted; a reload clears it). Unbounded in theory, harmless
  in practice; could prune ids whose interrupts have resolved.
- **`activeQuestion` scans ALL messages/parts per derivation** (`messages`
  changes identity every stream batch). Cheap improvement while porting: scan
  only the LAST assistant message — a pending question is always on the newest
  turn; older unresolved questions are stale by definition.

## 9. Browser verification checklist (run against the live stack)

Port-acceptance invariants: task rows never open an empty panel; the board
never vanishes mid-coordinator-stream; Dismiss makes zero network calls and
leaves the run `awaiting-input`; one mark-viewed POST per open; `kind:"file"`
never reaches a text renderer; no separate sessions section; nothing but the
streaming message body re-renders per token. Walk-through:

- [ ] Swarm run (e.g. `swarm_coordinator`): task board appears with spinners on
      running children; counts `done/total` tick up; board persists while the
      coordinator writes the final answer; board hides after settle.
- [ ] Click a task row mid-run → side panel streams the child's markdown live;
      panel header shows the `type#short` label + working/done badge.
- [ ] Agent cards show a step timeline (progress messages, dupes ×N); no
      inline streamed text; no jank while ~7k delta parts accumulate.
- [ ] Transcript contains no task/task_join/todowrite cards; reload OK.
- [ ] Question flow: composer swapped → answer → resume 200 → composer back →
      answered card inline. Dismiss → composer back, no POST, sidebar row shows
      amber exclamation → "Skipped Questions" → reopen → answer works. Reload
      mid-question re-surfaces it.
- [ ] Sidebar: green pulse running; amber exclamation awaiting-input; blue ring
      finished-unviewed; open thread → single viewed POST → gray ring. Hover:
      icon fades, trash in the same slot, delete works.
- [ ] Blur/refocus an open thread → one thread-messages refetch; tool cards
      present when opening a thread right as its run finishes.
- [ ] Office doc: "Create a PowerPoint titled Q3 Review…" → artifact chip (file
      icon) in transcript, FileCard in panel (name/type/size), Download 200 +
      valid file, Open-in-new-tab, persists across reload; a csv artifact still
      downloads via Blob.
- [ ] `tsc --noEmit` + eslint clean; profiler shows no per-token re-renders of
      TaskBar/AgentCards/sidebar.
