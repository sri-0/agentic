# Migration plan: porting the agentic + agentui feature stack into a fork

**Audience:** an autonomous coding agent (Claude Sonnet 4.6) working inside *forked* copies of
`agentic` (Go backend) and `agentui` (Next.js frontend). The forks diverged before this work
landed and carry their own local changes that MUST be preserved.

**Reference repos (read-only ground truth):**

- Backend: `github.com/sri-0/agentic`, branch `main` @ `17b1a87` — every feature described in
  docs 01–07 is merged there. All backend file:line references in this series refer to that
  commit unless a specific SHA is given.
- Frontend: `github.com/sri-0/agentui`, branch `main` @ `a5c544e` — docs 08–10. Old base
  (pre-stack) was `8cc25d0`.

**What this series is:** an implementation guide, not a patch queue. Each doc explains a
feature phase — intent, architecture, load-bearing code, implementation order, verification,
and the defects we found in review that the fork should fix *while porting* (the reference
repos still contain most of those defects; do not copy them faithfully).

---

## Prime directives for the migrating agent

1. **Never blind cherry-pick.** The reference commits assume a tree your fork does not have.
   Read the referenced code, understand the mechanism, re-implement it in the fork's idiom.
   Where the fork already has an equivalent (its own session store, its own sidebar), *adapt
   the mechanism to it* rather than replacing the fork's code wholesale.
2. **Preserve fork-local changes.** Before each phase, run `git log --oneline <fork-base>..HEAD`
   and `git diff <upstream-equivalent>` in the fork to inventory local changes touching the
   files that phase will modify. If a phase would overwrite a fork-local behavior, keep the
   fork's behavior and note the divergence in the phase's commit message.
3. **One phase = one branch = one PR**, in the order below. Do not start a phase until the
   previous one builds, passes tests, and passes its verification checklist. Stacked-branch
   chaos is how the original work needed a 30-PR review; the fork gets to do it cleanly.
4. **Implement the fixed versions.** Every doc has a "Known issues — fix during port" section.
   Those defects exist in the reference repos; the fork should land the corrected code the
   first time. When a doc says "SKIP" (e.g. `AgentLivePreview`, the separate Running-sessions
   sidebar section, the 1.2s refetch hack), do not build the thing at all — it was built and
   torn out upstream; the doc explains the surviving design.
5. **Port the tests with the code.** Where the reference has tests (`project_test.go`,
   `coordinator_test.go`, `reproduction_test.go`, question-tool round-trip tests), port them.
   Where a doc lists "tests the fork should ADD", write them — they cover the exact places
   upstream regressions actually occurred.
6. **Verify each phase end-to-end** with the doc's checklist before moving on. Backend phases
   have curl scripts; frontend phases have browser checklists. "Compiles" is not "done".

## Phase order and dependency graph

Backend first (the frontend consumes its wire contracts), then frontend. Within each repo the
order below is dependency order — do not reorder.

| # | Doc | Repo | Depends on | Summary |
|---|-----|------|-----------|---------|
| 01 | `01-swarm-baseline.md` | agentic | — | Typed roster, markdown agent defs, event-sourced sessions (Redis event log), task/question tools, MCP client, office MCP server |
| 02 | `02-run-framing-resume.md` | agentic | 01 | Run-framed event log (multi-turn, queueing, eviction), event-sourced resume, question answers, session authz, cancel |
| 03 | `03-swarm-visibility-governance.md` | agentic | 02 | Dynamic task-tool swarm dispatch, live child-event streaming, governance; deletes static swarm impls |
| 04 | `04-durability-history-memory.md` | agentic | 03 | Redis Lua atomic append, cold OpenSearch archive, `ProjectMessages` projection, post-run hooks, memory kNN fix + dedup |
| 05 | `05-mcp-oauth-office-artifacts.md` | agentic | 04 | MCP static + OAuth auth (optional sub-phase), resilient toolsets, office `/files`, file-artifact emission via StateDelta |
| 06 | `06-session-lifecycle-thread-persistence.md` | agentic | 04 | Thread docs on chat path, HITL prompt, `MAX_OUTPUT_TOKENS`, `SESSION_RETENTION`, viewed-state, synchronous terminal flush |
| 07 | `07-message-identity-session-load.md` | agentic | 06 | Deterministic message ids `{session}:{turn}:{role}`, async titles, session-aware messages API (`{data, live}`), metadata/reasoning persistence, Auto agent classifier |
| 08 | `08-frontend-foundation.md` | agentui | 02 | Identity seam (X-User-ID), sessions API + reconnect hook, question card, thread refresh, Stop→server-cancel |
| 09 | `09-frontend-swarm-question-sessions.md` | agentui | 03, 05, 06 | Swarm task bar + live sub-agent panel, question-replaces-composer + skip, 4-state thread-row status, file-artifact cards |
| 10 | `10-frontend-perf-resume-metadata.md` | agentui | 07, 09 | Memo-comparator perf pattern, tail-only mid-stream resume, reasoning-duration/metadata rehydration, Auto in picker, question pager |

Phases 05's OAuth half is **optional** — upstream never wired the per-user token injection into
the run path (see doc 05); the fork can defer the whole OAuth sub-phase without losing anything
that currently works.

## Wire contracts (the seams between repos)

The frontend/backend seam is small and explicit. If the fork keeps these contracts, the two
sides can be ported independently after phase 02:

- **AI-SDK data-stream protocol** over SSE from `POST /v1/chat/completions` (`stream: true`,
  `agent_id`, `thread_id`, `X-User-ID`). Custom part types: `data-agent-delta` (keyed
  `"<type>#<shortid>"`), `data-agent-progress`, `data-task-list`, `data-artifact`
  (incl. `kind:"file"` with `url/filename/mime/size`), tool interrupts
  (`tool-approval-request`, `data-tool-interrupt` with `questions`).
- **Message identity:** `{session}:{turn}:{role}` stamped on the live start frame, the archived
  doc, and the replay projection alike (doc 07 — this invariant is what makes reload/resume
  dedup-free; treat it as non-negotiable).
- **Sessions API:** `GET /v1/sessions` (+ per-id status incl. `start_seq`, `turn`, `viewed`),
  `POST /v1/sessions/{id}/cancel`, `POST /v1/sessions/{id}/viewed`, follow endpoint with
  `?after=<seq>` replay.
- **Threads API:** `GET /v1/threads`, `GET /v1/threads/{id}/messages` returning either a bare
  array (settled) or `{data, live:{head_seq,turn,status}}` (in-progress) — doc 07/10.
- **Resume:** `POST /v1/agent/resume` `{thread_id, action: approved|denied|skipped, answers?}`.

## Cross-cutting fixes (apply throughout, not per-phase)

These came out of the whole-codebase review; bake them in as you go:

1. **Ownership gates on ALL thread endpoints.** Upstream gates reads but not mutations
   (update/delete/message-create/bulk/delete) — a real cross-user exposure. Gate everything
   from the start (doc 04 has the pattern).
2. **No silent fail-open.** Upstream swallows errors to defaults in many places (memory dedup
   lookup, event-log appends, viewed lookups, frontend queryFns). Each was individually
   defensible; collectively they hid a system-wide recall outage for weeks. Log every
   degradation at warn.
3. **Named constants for cross-file invariants:** `"New Chat"`, `"office:artifact:"`,
   `"auto"`, the `{session}:{turn}:{role}` format string. Upstream repeats each as literals
   in 3–5 files with "MUST match" comments.
4. **Config through one surface.** Backend env vars go through `internal/config`
   (upstream strays: `MAX_OUTPUT_TOKENS`, `MCP_OAUTH_REDIRECT_URL`, `ENCRYPTION_KEY`).
5. **Golden test for the encoder/projector mirror.** The live SSE encoder, the replay
   projector, and the archiver must emit identical shapes; upstream keeps them in sync by
   comments alone. Add one test that renders the same event sequence through both and diffs.

## Verification infrastructure

Each backend phase assumes: Docker (Valkey :6379, OpenSearch :9200 — `docker-compose.yaml` at
the repo root), `.env` with an OpenAI-compatible gateway key (`LLM_BASE_URL`/`LLM_API_KEY`),
office MCP on :8090 (`services/office-mcp`, needs a venv with its requirements). Backend
`PORT=8011`, frontend `.env.local` pointing at it. `make test` after every phase; the only
acceptable pre-existing failure upstream is `TestStreamAgentRun_SubAgentToolAttribution`.

Frontend phases: `npx tsc --noEmit` must stay clean; add `ds-bundle/`-style vendored dirs to
eslint ignores so lint stays signal, not noise.
