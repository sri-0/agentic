# Agent Swarm Re-Architecture — Plan Index

This folder contains the per-phase implementation plans for re-architecting the `agentic` agent layer on opencode / Claude Code patterns. The master plan (context, locked decisions, architecture overview) lives at `~/.claude/plans/quiet-finding-breeze.md`; this index + the phase files are the executable breakdown.

## Locked decisions (summary)

1. Re-architect orchestration; keep **adk-go as the leaf executor**.
2. Subagent model = **task tool + typed roster**, each spawn a **child session**.
3. **Own the glue, ADK everywhere else** (LlmAgent/functiontool/MCP/Runner for leaves; AgentTool for sync consults; Sequential/Parallel/Loop for static pipelines).
4. **Gated nesting** (leaves by default; `task` granted per-type; depth-capped).
5. **Event-sourced** sessions; exactly-once `?after=<seq>` resume; survives disconnect + restart.
6. **EventLog port** → Redis Streams now, **Kafka later** (no rewrite); flush to OpenSearch.
7. **Todo = per-session state** (`todowrite` + synthesised from spawns), not an agent.
8. **Two-tier memory** (Redis short-term + compaction; OpenSearch kNN long-term, per-user, auto + `add_memory`).
9. Internal agents: **compaction, memory-extractor, title/summariser, suggestions, router** (router separate from suggestions).
10. **MCP auth = backend-held tokens** (browser only redirects).
11. **Question tool** mirrors opencode exactly; resolved via `/v1/agent/resume`.
12. **Office docs = Python MCP server** (docxtpl + pptx-mcp fork + excel-mcp), returns URLs.
13. **Identity = `userID` from header**, default `anonymous`, one resolver seam.
14. Per-phase plans, full-stack, UI plans cross-linked.

## Phases

| # | File | Goal | Depends on |
|---|------|------|------------|
| 00 | [00-foundations.md](00-foundations.md) | Typed registry, leaf consolidation, permissions, identity seam | — |
| 01 | [01-sessions-streaming.md](01-sessions-streaming.md) | Background runs, event-sourced log, resume, full-parts history | 00 |
| 02 | [02-swarm.md](02-swarm.md) | task tool, child sessions, coordinator, RunBus | 00, 01 |
| 03 | [03-memory-internal-agents.md](03-memory-internal-agents.md) | Two-tier memory + 5 internal agents | 00, 01 |
| 04 | [04-mcp.md](04-mcp.md) | MCP integration + backend-held OAuth | 00, 02 |
| 05 | [05-question-agents.md](05-question-agents.md) | Interactive question tool | 01, 02 |
| 06 | [06-office-docs.md](06-office-docs.md) | Office-doc Python MCP server | 04 |
| 07 | [07-frontend-contract.md](07-frontend-contract.md) | agentui changes (mirrors to `agentui/plans/swarm/`) | 01, 02, 05 |

## Execution order

00 → 01 → 02 are the spine (substrate → sessions → swarm). 03/04/05 layer on top and can be interleaved. 06 rides 04. 07 tracks the wire contracts from 01/02/05 and is mirrored into the `agentui` repo.
