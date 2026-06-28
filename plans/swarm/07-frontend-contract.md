# Phase 07 — Frontend contract (agentui)

> The agentui changes the backend phases require. Mirrored into `agentui/plans/swarm/` and cross-linked to each backend phase.

Depends on the wire contracts from: [01](01-sessions-streaming.md), [02](02-swarm.md), [05](05-question-agents.md), [04](04-mcp.md).

## Current state (verified)

agentui (Next.js 16, AI SDK v6) consumes a **native AI SDK UI message stream** from `POST /v1/chat/completions?format=aisdk` (no BFF/proxy). It already renders `data-agent-step`/`data-agent-delta`/`data-agent-progress`/`data-task-list`/`data-artifact`/`data-usage` parts and agent cards keyed by `agentId` (`GET /v1/agents`). Gaps: history rehydration is **text-only** (`lib/chat/from-history.ts` drops reasoning/tools/artifacts/sub-agent state on reload); **no stream-resume**; **no auth**.

## Changes

1. **Full-parts history rehydration** (Phase 01). Upgrade `lib/chat/from-history.ts` + the `GET /v1/threads/{id}/messages` consumer to reconstruct full AI-SDK `parts` (reasoning/tools/artifacts/sub-agent cards/task list) — the backend now returns `parts` (via `ProjectMessages`), not text-only. Reload must render identically to live.
2. **Stream resume** (Phase 01). Track the high-water `seq`; on reload/network drop, reconnect to `GET /v1/sessions/{id}/stream?after=<seq>` (instead of dropping the in-flight stream). Use the AI SDK transport's resumable-stream support.
3. **Sessions sidebar** (Phase 01). Consume `GET /v1/sessions` to show **still-running** sessions with a live badge; join with `/v1/threads` by id. Lets the user leave and rejoin a running swarm.
4. **Swarm cards** (Phase 02). Confirm `data-agent-*` + `data-task-list` handle the new metadata link (coordinator `task` tool-call part → child card via `childSessionId`/`subagentType`) and multi-instance keys (same type spawned N times, disambiguated by child session id). Cards already largely exist.
5. **Question cards** (Phase 05). New `data-question` `ChatDataParts` entry + renderer (options + optional free-text), and an **answer round-trip** extending the HITL resume mechanism (`/v1/agent/resume` with `{thread_id, request_id, answers}`).
6. **MCP UI** (Phase 04). "Connect server" flow (`POST /v1/mcp/{server}/connect` → redirect to provider → backend callback), `needs_auth` state surfacing, connected-server list.
7. **Identity** (Phase 00). Send the `X-User-Id` header from `apiFetch` + the chat/resume transports (default `anonymous` until real auth).

## Preserve (hard constraints)

snake_case stream body keys (`messages, agent_id, model, thread_id, use_rag, temporary, reasoning_effort`); native `text`/`reasoning`/`dynamic-tool` part types; the `approval-requested` tool state; keyed-replacement semantics (same-`id` re-emit replaces). Keep `agentui/AGENTS.md` §4–6 and `lib/chat/types.ts` in sync with the backend `internal/stream/aisdk` encoder (the authoritative spec).

## Files (agentui)

`lib/chat/{from-history,api,types}.ts`; `components/chat/*` (message-list, agent-cards, task-bar); a sessions sidebar; a `data-question` renderer; MCP-connect components; `lib/api/client.ts` (X-User-Id header).

## Verification

All backend features render correctly and **survive reload** (full parts, mid-stream resume); still-running sessions appear in the sidebar and rejoin live; question cards round-trip; MCP connect works; existing chats unaffected; `AGENTS.md` + `types.ts` match the encoder.
