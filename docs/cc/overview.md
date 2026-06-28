Entry point: src/main.tsx (Commander.js CLI)
Core engine: src/QueryEngine.ts (~46K lines) — LLM request orchestration
Tool type system: src/Tool.ts (~29K lines)

---
Agent / Sub-Agent Architecture

Claude Code implements a swarm-based multi-agent system with three deployment modes:

1. In-Process Teammates

Spawned in the same Node process with shared state. Used for parallel work within a session. Managed via src/utils/swarm/spawnInProcess.ts.

2. Tmux-Spawned Agents

External terminal sessions via src/utils/swarm/backends/TmuxBackend.ts. Full isolation with their own process. CLI flags (permission mode, model, plugins) are inherited via buildInheritedCliFlags().

3. Async Background Agents

Fire-and-forget tasks that report back via task notifications.

Coordinator Mode

Enabled via CLAUDE_CODE_COORDINATOR_MODE=1:
- Main agent acts as coordinator (can only use AgentTool, TaskStopTool, SendMessageTool, SyntheticOutputTool)
- Spawns worker agents that do the actual file editing/searching
- Workers report back via <task-notification> XML messages
- Coordinator synthesizes results

Key Agent Files

┌──────────────────────────────────────────┬─────────────────────────────────────────┐
│                   File                   │                 Purpose                 │
├──────────────────────────────────────────┼─────────────────────────────────────────┤
│ src/tools/AgentTool/AgentTool.tsx (234K) │ Main agent spawning tool                │
├──────────────────────────────────────────┼─────────────────────────────────────────┤
│ src/tools/AgentTool/runAgent.ts          │ Agent lifecycle execution               │
├──────────────────────────────────────────┼─────────────────────────────────────────┤
│ src/tools/AgentTool/agentToolUtils.ts    │ Tool resolution & filtering per agent   │
├──────────────────────────────────────────┼─────────────────────────────────────────┤
│ src/tools/AgentTool/loadAgentsDir.ts     │ Load custom agent definitions from disk │
├──────────────────────────────────────────┼─────────────────────────────────────────┤
│ src/tools/AgentTool/forkSubagent.ts      │ Fork agents with cached prompts         │
├──────────────────────────────────────────┼─────────────────────────────────────────┤
│ src/coordinator/coordinatorMode.ts       │ Coordinator orchestration mode          │
├──────────────────────────────────────────┼─────────────────────────────────────────┤
│ src/utils/swarm/ (21 files)              │ Swarm infrastructure                    │
└──────────────────────────────────────────┴─────────────────────────────────────────┘

Agent Definition Structure

Each agent has: name, type (builtIn/custom/worker), description, tools, disallowedTools, permissionMode, systemPrompt. Built-in agents include "researcher", "implement", "verify", "worker".

Context Forking

When a subagent spawns:
- File state cache is cloned (reads shared, writes isolated)
- Permission context is cloned with same rules
- System prompt is shared (for prompt cache stability)
- setAppState → no-op for async agents, routes to parent for in-process
- New AbortController per agent

Inter-Agent Communication

A mailbox system (src/context/mailbox.tsx) handles:
- Worker → Leader: permission requests, results
- Coordinator → Workers: SendMessage tool
- Workers → Coordinator: task notifications

---
Complete Tool Inventory (~50 tools)

File Operations

┌──────────────────┬──────────────────────────────────────┐
│       Tool       │             Description              │
├──────────────────┼──────────────────────────────────────┤
│ BashTool         │ Shell command execution              │
├──────────────────┼──────────────────────────────────────┤
│ FileReadTool     │ Read files (images, PDFs, notebooks) │
├──────────────────┼──────────────────────────────────────┤
│ FileWriteTool    │ Create/overwrite files               │
├──────────────────┼──────────────────────────────────────┤
│ FileEditTool     │ Partial file edits                   │
├──────────────────┼──────────────────────────────────────┤
│ NotebookEditTool │ Jupyter notebook editing             │
└──────────────────┴──────────────────────────────────────┘

Search & Discovery

┌────────────────┬──────────────────────────┐
│      Tool      │       Description        │
├────────────────┼──────────────────────────┤
│ GlobTool       │ File pattern matching    │
├────────────────┼──────────────────────────┤
│ GrepTool       │ Content search (ripgrep) │
├────────────────┼──────────────────────────┤
│ ToolSearchTool │ Discover deferred tools  │
└────────────────┴──────────────────────────┘

Web

┌────────────────┬────────────────────────────────────────┐
│      Tool      │              Description               │
├────────────────┼────────────────────────────────────────┤
│ WebFetchTool   │ Fetch URL content                      │
├────────────────┼────────────────────────────────────────┤
│ WebSearchTool  │ Web search                             │
├────────────────┼────────────────────────────────────────┤
│ WebBrowserTool │ Full browser (gated: WEB_BROWSER_TOOL) │
└────────────────┴────────────────────────────────────────┘

Agent & Task Management

┌─────────────────┬───────────────────────────────┐
│      Tool       │          Description          │
├─────────────────┼───────────────────────────────┤
│ AgentTool       │ Spawn sub-agents              │
├─────────────────┼───────────────────────────────┤
│ TaskCreateTool  │ Create tasks (gated: TODO_V2) │
├─────────────────┼───────────────────────────────┤
│ TaskUpdateTool  │ Update task status            │
├─────────────────┼───────────────────────────────┤
│ TaskGetTool     │ Get task details              │
├─────────────────┼───────────────────────────────┤
│ TaskListTool    │ List tasks                    │
├─────────────────┼───────────────────────────────┤
│ TaskStopTool    │ Terminate an agent            │
├─────────────────┼───────────────────────────────┤
│ TeamCreateTool  │ Create swarm teams            │
├─────────────────┼───────────────────────────────┤
│ TeamDeleteTool  │ Delete swarm teams            │
├─────────────────┼───────────────────────────────┤
│ SendMessageTool │ Inter-agent messaging         │
└─────────────────┴───────────────────────────────┘

Special Purpose

┌────────────────────────────────────────────────┬──────────────────────────────────────────┐
│                      Tool                      │               Description                │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ SkillTool                                      │ Invoke registered skills                 │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ EnterPlanModeTool / ExitPlanModeTool           │ Reversible edit planning mode            │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ EnterWorktreeTool / ExitWorktreeTool           │ Git worktree isolation                   │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ REPLTool                                       │ Virtual terminal VM (internal only)      │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ LSPTool                                        │ Language server (gated: ENABLE_LSP_TOOL) │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ MCPTool                                        │ Model Context Protocol tools             │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ CronCreateTool / CronDeleteTool / CronListTool │ Scheduled triggers                       │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ RemoteTriggerTool                              │ Remote agent triggers                    │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ AskUserQuestionTool                            │ Block for user input                     │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ SyntheticOutputTool                            │ Structured output generation             │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ SleepTool                                      │ Wait (gated: PROACTIVE/KAIROS)           │
├────────────────────────────────────────────────┼──────────────────────────────────────────┤
│ TodoWriteTool                                  │ Legacy todo operations                   │
└────────────────────────────────────────────────┴──────────────────────────────────────────┘

---
Tool Access Control by Agent Type

The tool filtering pipeline works in stages:

getAllBaseTools()          ← all tools respecting feature flags
→ getTools()            ← apply permission deny rules + mode filters
    → filterToolsForAgent() ← agent-specific restrictions
    → Final tool pool sent to model

Disallow Lists (src/constants/tools.ts)

ALL subagents cannot use:
- AskUserQuestionTool, TaskOutputTool, ExitPlanModeTool, EnterPlanModeTool, TaskStopTool
- AgentTool (blocked unless internal Anthropic user)

Custom agents (non-built-in) additionally cannot use:
- Everything in the ALL list above (inherited)

Async/background agents are restricted to ONLY:
- FileRead, FileEdit, FileWrite, Bash, Grep, Glob, WebSearch, WebFetch, NotebookEdit, Skill, SyntheticOutput, ToolSearch, EnterWorktree, ExitWorktree, TodoWrite

In-process teammates can ONLY use:
- TaskCreate, TaskGet, TaskList, TaskUpdate, SendMessage
- Plus CronCreate/Delete/List if triggers are enabled

Coordinator mode agents can ONLY use:
- AgentTool, TaskStopTool, SendMessageTool, SyntheticOutputTool

---
Guardrails & Safety System

1. Three-Tier Permission Decision Flow

Tool.checkPermissions()
├─ Hook check: executePermissionRequestHooks()
├─ Rule matching: rules from 7 sources (see below)
├─ LLM classifier: yoloClassifier (auto mode only)
└─ Interactive prompt: show dialog → user decides

2. Permission Sources (in priority order)

1. policySettings — Organization-level policies (highest priority)
2. userSettings — ~/.claude/settings.json
3. projectSettings — .claude/project.json
4. localSettings — .claude/local.json
5. flagSettings — Feature flags (GrowthBook)
6. cliArg — Command-line flags
7. session — Temporary session rules

3. Permission Modes

┌───────────────────┬──────────────────────────────────────────────┐
│       Mode        │                   Behavior                   │
├───────────────────┼──────────────────────────────────────────────┤
│ default           │ Ask for dangerous operations                 │
├───────────────────┼──────────────────────────────────────────────┤
│ plan              │ Atomic, reversible edits only                │
├───────────────────┼──────────────────────────────────────────────┤
│ bypassPermissions │ Allow everything                             │
├───────────────────┼──────────────────────────────────────────────┤
│ acceptEdits       │ Auto-accept file edits, ask for bash         │
├───────────────────┼──────────────────────────────────────────────┤
│ dontAsk           │ Never prompt (deny if not allowed)           │
├───────────────────┼──────────────────────────────────────────────┤
│ auto              │ LLM classifier auto-approves safe operations │
└───────────────────┴──────────────────────────────────────────────┘

4. Bash-Specific Guardrails

- Command parsing: Splits pipes, redirections, aliases
- Dangerous pattern detection: rm -rf /, sudo, wget to /dev, etc.
- Output redirection analysis: Prevents data exfiltration via >
- Sandbox enforcement: Restricts cd outside working directory
- Pattern matching: Rules like "git *", "npm run test" for granular control

5. Filesystem Guardrails (src/utils/permissions/filesystem.ts, 62K)

- Path validation: Resolves symlinks, detects escape attempts
- Working directory scope: Tools can only access files within the project
- Protected namespaces: .git/, .claude/, shell config files are shielded
- Scratchpad directory: Shared cross-worker space in coordinator mode (controlled)

6. YOLO Classifier (src/utils/permissions/yoloClassifier.ts, 52K)

In auto permission mode, an LLM-based classifier decides whether to auto-approve:
- Two-stage: Fast prompt check → extended thinking check
- Transcript formatting: Sends recent conversation context to classifier
- Cache token tracking: Monitors cost
- Fallback: On API error or uncertainty → falls back to interactive prompting

7. Denial Tracking

- Tracks consecutive denials per user
- After 2 consecutive denials → falls back to interactive prompting
- Prevents infinite deny loops in auto mode

8. Hook System

Pre/post tool use hooks defined in settings.json:
- Can allow, deny, or ask any tool use
- Access to tool name, input, working directory
- Can suggest permission updates
- Hooks from <user-prompt-submit-hook> treated as user input

9. Swarm Permission Bridge

- Subagents cannot make permission decisions themselves
- Permission requests are forwarded to the parent/leader via mailbox
- Parent decides and sends response back
- Ensures human stays in the loop even with nested agents

---
Key Architectural Patterns

1. Lazy module loading — Heavy modules (OpenTelemetry, gRPC) only load when needed via feature flags and bun:bundle dead code elimination
2. Prompt cache optimization — Tool ordering is deterministic, system prompts are shared/cached across agents
3. React/Ink terminal UI — Full React component tree for the terminal interface (~140 components)
4. Zustand-like state — AppStateStore with selectors and immutable updates
5. Context forking — Each subagent gets an isolated clone of parent context with controlled sharing