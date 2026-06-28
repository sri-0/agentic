# Phase 03 — Memory + internal agents

> Two-tier memory + the five internal (non-spawnable) system agents.

Depends on: 00 (registry, internal-runner pattern), 01 (run lifecycle, context-window signals). Can interleave with 04/05.

## Current state (verified)

- **Long-term memory tools** — `pkg/memory` service backed by OpenSearch, exposed as `add_memory`/`search_memories`/… tools + `/v1/memories` CRUD.
- **Session memory + compaction** — `internal/memory`: `SessionMemory` (structured notes in Valkey, key `session_notes:userId:threadId`, 7-day TTL, 12k-token cap) + `CompactionService` (full/partial/up-to summarization). Both run via the **internal agents registry** (`internal/agents`) using ADK runners with a cheap model (prefers `gpt-4o-mini`) + prompt templates `config/default/prompts/*.tmpl`.
- `/v1/embeddings` route exists.

## Design

### Two-tier memory

**Short-term (Redis / Valkey):**
- Rolling session notes — reuse `internal/memory:SessionMemory` (token-capped, TTL), keyed by `userID:threadID`.
- **Compaction** internal agent — rolling, **anchored** summary (opencode pattern): keep the most recent ~2 turns verbatim (token-bounded), summarise the head, **feed the previous summary forward** (anchored, not fresh each time). Fixed sectioned template (Goal / Constraints / Progress / Key Decisions / Next Steps / Critical Context / Relevant Files). Trigger when `used >= context_window − reserved`. **Never delete history** — change only the *projection* sent to the model (reorder to `[summary] + [recent tail] + [new]`). Reuse the existing `CompactionService` + internal-runner pattern; align the trigger with the Phase-01 usage signal.

**Long-term (OpenSearch, per-user, semantic kNN):**
- Written by **both** (a) a **memory-extractor** internal agent (extracts durable facts after a session) and (b) the explicit **`add_memory`** tool (reuse `pkg/memory` + existing memory tools).
- Recall via OpenSearch kNN, injected into context on the relevant run path (extend the existing retrieval/RAG augmentation in `handler/chat.go`). Scope = `userID`.
- Requires embeddings — confirm the embedding model/route (reuse `/v1/embeddings`). **Verify the OpenSearch memory index mapping has a kNN vector field** (`knn_vector`); add it if missing in `pkg/db/opensearch/indices.go`.

### Internal agents (`internal/agents` registry — not spawnable, not in `/v1/agents`)

1. **Compaction** — above.
2. **Memory-extractor** — above; runs post-session (or on a cadence), writes per-user long-term memories.
3. **Title / Summariser** — thread titles + 1-line recaps (drives the sessions sidebar + thread list). Cheap model.
4. **Auto-suggestions** — next-step suggestion chips after an assistant turn.
5. **Auto-router / classifier** — picks which **primary** agent should handle a request (the "Auto" mode `agentui/plans/06-auto-router-classifier.md` sketches). **Separate agent from suggestions.** Consumed by `handler/chat.go` routing when the request selects "Auto" (or no explicit `agent_id`).

All five reuse the existing internal-runner pattern (cheap model + prompt templates), are registered in `internal/agents`, and are excluded from the `roster.Registry.Primary()`/`Dispatchable()` surfaces so they never appear in `/v1/agents` or as `task()` targets.

## Files

**Extend:** `internal/memory/*` (compaction trigger wired to Phase-01 usage; projection reorder), `internal/agents/*` (add memory-extractor, title, suggestions, router definitions + prompt templates in `config/<env>/prompts/`), `pkg/memory/*` (kNN recall helper).
**Modify:** `internal/handler/chat.go` (kNN recall injection; "Auto" routing via the router agent), `pkg/db/opensearch/indices.go` (ensure `knn_vector` mapping).

## Verification

- Compaction triggers near the context limit; the conversation continues coherently (no "context was compacted" mention; recent turns intact).
- `add_memory` a fact, then a **new** session recalls it via kNN injection.
- Memory-extractor populates per-user memories after a session.
- Title/suggestions produce sane output; the router picks the right primary agent for a few representative prompts.
- Unit tests: compaction projection (head summarised, tail verbatim, prior summary anchored); kNN recall scoping by userID.
