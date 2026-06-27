# AGENTS.md — `agentic`

An LLM **agentic proxy / agent server** written in Go. It exposes an
OpenAI-compatible HTTP API in front of a tree of agents built on
[`google.golang.org/adk`](https://pkg.go.dev/google.golang.org/adk) (the Agent
Development Kit, "ADK"). Requests are either **routed to a registered agent**
(single LLM, sequential, parallel, loop, swarm/coordinator) or **proxied
straight through** to an upstream model provider. Agent activity is streamed to
clients over SSE in one of two wire formats.

The companion frontend lives in `~/code/agentui` (Next.js). It talks to this
server directly using the `aisdk` stream format.

---

## 1. Quick start

```bash
# Dependencies: OpenSearch (vectors/docs), Valkey (sessions/HITL), optional Confluence
docker compose up -d            # opensearch:9200, dashboards:5601, valkey:6379, redis-insight:5540

cp .env .env.local              # set API keys (LLM_API_KEY / OPENROUTER / ANTHROPIC etc.)
make seed                       # seed OpenSearch indices with sample docs/prompts/threads
make seed-confluence            # (optional) seed a sample Confluence space

make server                     # production OpenAI-compatible server (default :8000)
make dev                        # ADK dev web UI (:8080)
make cli                        # interactive terminal chat
make test                       # go test ./...
```

`Makefile` targets: `server`, `dev`, `dev-agent AGENT=…`, `cli`, `cli-agent AGENT=…`,
`build` (→ `bin/`), `test`, `seed`.

> The `README.md` is a one-liner ("Some vibe coded junk"). **This file is the
> real documentation — keep it current when you change protocols, config schemas,
> or the agent registry.**

---

## 2. Repository layout

```
cmd/
  server/          # production HTTP server (OpenAI-compatible API)
  dev/             # ADK dev web UI launcher (per-agent debugging)
  cli/             # interactive terminal chat
  seed/            # seed OpenSearch: embeddings, prompts, threads, messages
  seed-confluence/ # seed a sample Confluence space + pages

internal/
  bootstrap/       # Init(): load config, connect deps, build the agent registry
  config/          # config structs + YAML loaders (config.go, models.go, agents.go, rag.go)
  server/          # gorilla/mux router, CORS + logging middleware
  handler/         # HTTP handlers (chat, resume, models, agents, threads, prompts, …)
  agent/           # Core, SessionManager, the SSE streaming event loop (stream.go)
  agents/          # *internal* system agents (compaction, session memory, suggestions)
  stream/          # wire-format encoders: stream.go (Sink/Format), openai/, aisdk/
  sse/             # low-level SSE frame encoder
  chat/            # message persistence (async OpenSearch indexing), prompt templating
  tasks/           # task-board model for swarm/coordinator → UI task-list events
  hitl/            # human-in-the-loop pending-interrupt store (memory or valkey)
  tools/           # agent tool registry + implementations (+ tools/confluence)
  rag/             # retrieval: query embedding, OpenSearch vector/text search
  memory/          # thin internal memory interface (NoOp default)
  prompts/         # text/template loader for *.tmpl prompt files
  proxy/           # raw upstream pass-through (OpenUpstream / ForwardTo)
  anthropic/       # Anthropic native /v1/messages request/response/SSE types
  types/           # public API request/response types (ChatCompletionRequest, …)
  logbridge/       # bridge ADK/library logs into zerolog

agents/            # the user-facing AGENT IMPLEMENTATIONS (the heart of the repo)
  shared/          # ResolveProvider, ResolveTools, BuildLLMAgent, RequireSubAgent, …
  basic/  explore/  plan/  verification/  codeguide/   # single LLM agents
  triage/                                              # sequential + parallel pipeline
  deepresearch/                                        # deep multi-stage pipeline + code agents
  swarm/  coordinator/                                 # dynamic multi-agent dispatch

pkg/               # reusable, app-agnostic packages
  db/opensearch/   # OpenSearch client + index definitions
  db/valkey/       # Valkey (Redis) client + KV helpers
  session/valkey/  # ADK session.Service backed by Valkey
  genai/anthropic/ # ADK model.LLM impl for Anthropic
  genai/openai/    # ADK model.LLM impl for OpenAI-compatible APIs
  memory/          # long-term memory service + ADK memory tools (OpenSearch-backed)
  openapi/         # OpenAPI spec generation
  logging/         # zerolog setup + HTTP logging middleware

config/
  default/         # active config: agents.yaml, models.yaml, rag.yaml, prompts/*.tmpl
  claude-code/     # reference Claude-Code-style agent definitions (main/coordinator/…)

.plans/01-stream.md  # design notes comparing Claude/Codex/Responses/AI-SDK stream protocols
docs/cc/overview.md  # notes on Claude Code's own internal architecture
```

---

## 3. Request lifecycle (chat)

`POST /v1/chat/completions` → `handler.Chat` (`internal/handler/chat.go`):

1. **Parse** body into `types.ChatCompletionRequest` (§6). Accepts both OpenAI
   `{role, content}` messages and AI-SDK `UIMessage` `{role, parts:[…]}` (text
   parts are concatenated).
2. **Pre-process**: apply a prompt template (`prompt_id`) and/or RAG-augment the
   messages (`use_rag`).
3. **Route**: resolve the target agent id from `agent_id`/`agentId`, falling back
   to `model`. `registry.Get(agentID)`:
   - **hit** → run the agent (this server's core path);
   - **miss** → proxy the raw body upstream via `internal/proxy`.
4. **Thread/session**: if `thread_id` set and not `temporary`, attach a
   `chat.MessageSaver` (async OpenSearch persistence). Otherwise an
   `anon-<uuid>` thread id is generated.
5. **Stream vs not**: `stream` defaults to **true**.
   - non-stream → `agent.NonStreamAgentRun` (returns a single JSON
     `chat.completion`);
   - stream → `agent.StreamAgentRunFormat` with `format` from the
     `?format=` query param (§5).

The streaming path (`internal/agent/stream.go`) creates an encoder, ensures an
ADK session exists (`SessionManager.GetOrCreate`), saves the user message, then
runs the ADK runner and translates every ADK event into wire frames:

```go
for event, err := range core.Runner.Run(ctx, "default", threadID, userContent,
        adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}) {
    // dispatch on event.Content.Parts: text / thought / FunctionCall / FunctionResponse
    // dispatch on event.Actions.StateDelta: task-board snapshots
}
```

Key dispatch rules:
- **Output agent vs sub-agent.** `Core.OutputAgent` names the agent whose text is
  the user-visible answer. Its text/reasoning/tools stream as *main-thread*
  events (`enc.Text`, `enc.ToolCall`, …). Every *other* agent's activity streams
  as *attributed sub-agent* events (`enc.AgentText`, `enc.AgentToolCall`, …) so
  the UI can render per-agent cards.
- **Special tool responses**: `emit_artifact` → `enc.Artifact`; `todowrite` →
  `enc.TaskList`. Task boards in `StateDelta` (swarm/coordinator) →
  de-duplicated `enc.TaskList` snapshots (`internal/tasks`).
- **HITL**: a `FunctionCall` named `adk_request_confirmation` pauses the stream
  and emits a tool-interrupt (§7).
- **Finalize**: persist assistant text, emit usage + context breakdown, metadata,
  then run-finished + `[DONE]`.

---

## 4. HTTP API

Router: `internal/server/server.go` (`gorilla/mux`). Global middleware adds
permissive **CORS** (`Access-Control-Allow-Origin: *`, headers include
`X-User-ID`) and request logging. `OPTIONS` short-circuits to `200`.
Server timeouts: read `30s`, **write `5m`** (long SSE), idle `120s`. Default
port **`:8000`** (`PORT`/`HOST` env).

### Always available
| Method | Path | Handler | Purpose |
|---|---|---|---|
| GET | `/health` | `Health` | status + registered agent ids |
| GET | `/v1/models` | `Models` | list models (from `models.yaml`) + agents |
| GET | `/v1/agents` | `Agents` | list agents with full config |
| GET | `/v1/openapi.json` | `OpenAPISpec` | generated OpenAPI spec |
| GET | `/docs` | `APIDocs` | API docs page |
| POST | `/v1/chat/completions` | `Chat` | **main entry** — agent run or upstream proxy |
| POST | `/v1/messages` | `Messages` | Anthropic-native `/messages` adapter |
| POST | `/v1/embeddings` | `Embeddings` | embeddings proxy |
| POST | `/v1/agent/resume` | `Resume` | **HITL** approve/deny → resume stream |

### Conditional (registered only when OpenSearch is connected)
- **RAG**: `POST /v1/rag/search`
- **Prompts** (CRUD): `/v1/prompts`, `/v1/prompts/{id}`
- **Threads** (CRUD): `/v1/threads`, `/v1/threads/{id}`
- **Thread messages**: `/v1/threads/{id}/messages` (+ `/bulk`, DELETE)
- **Skills** (CRUD): `/v1/skills`, `/v1/skills/{id}`
- **Memories**: `/v1/memories`, `/v1/memories/search`, `/v1/memories/{id}`

### Conditional (registered only when internal agents exist)
- `POST /v1/suggestions` — next-prompt suggestions

User scoping for thread/memory/skill endpoints is via the **`X-User-ID`** header
(defaults to `anonymous`). There is no global auth yet.

---

## 5. Streaming wire protocols (SSE)

Every streamed response is `text/event-stream` with frames `data: {json}\n\n`,
terminated by `data: [DONE]`. The **format is selected by the `?format=` query
param**, defaulting to OpenAI (`internal/stream/stream.go`):

```go
const ( FormatOpenAI Format = "openai"; FormatAISDK Format = "aisdk" )
// ParseFormat: "aisdk"|"ai-sdk"|"ai_sdk"|"vercel" → FormatAISDK, else FormatOpenAI
```

Both formats are driven by the same `Encoder` interface and `Sink`
(`internal/stream/stream.go`); only the on-wire shapes differ. The two encoders
emit the **same logical events** — pick by client:

| Logical event | OpenAI format (`internal/stream/openai`) | AI-SDK format (`internal/stream/aisdk`) |
|---|---|---|
| run start/finish | `ag_ui.type: RUN_STARTED` / `RUN_FINISHED` | `{"type":"start"}` / `{"type":"finish"}` |
| main text | `choices[0].delta.content` | `text-start` / `text-delta` / `text-end` |
| main reasoning | `choices[0].delta.reasoning` (OpenRouter-style) | `reasoning-start` / `reasoning-delta` / `reasoning-end` |
| main tool call | `choices[0].delta.tool_calls[]` + finish | `tool-input-start` / `tool-input-delta` / `tool-input-available` |
| main tool result | `{"tool_result":{toolCallId,toolName,result}}` + `ag_ui TOOL_CALL_RESULT` | `tool-output-available` |
| run progress | `{"agent_progress":{phase,message}}` + `ag_ui CUSTOM` | `data-agent-progress` |
| sub-agent start/done | `agent_progress` phase `agent_start`/`agent_done` + `ag_ui STEP_*` | `data-agent-step` (`status: started`/`done`, `durationMs`) |
| sub-agent text/reasoning | `{"agent_event":{type:text_delta\|reasoning_delta,…}}` | `data-agent-delta` (`kind: text`/`reasoning`) |
| sub-agent tool call | `{"agent_event":{type:tool_call,…}}` | (folded into agent-delta/step stream) |
| artifact | `ag_ui CUSTOM name:artifact` | `data-artifact` |
| task list | `ag_ui CUSTOM name:task_list` | `data-task-list` |
| usage / context | `ag_ui CUSTOM name:context_usage` | `data-usage` |
| **tool interrupt (HITL)** | tool_call + `{"tool_interrupt":{…}}` + `ag_ui CUSTOM name:tool_interrupt` | tool_call + `tool-approval-request` + `data-tool-interrupt` |
| metadata | (in chunks) | `message-metadata` (model, agentId, durationMs) |

**OpenAI format** is an OpenAI `chat.completion.chunk` base with an extra
`ag_ui` sidecar object (loosely [AG-UI](https://github.com/ag-ui-protocol)-shaped)
plus flat custom keys (`agent_progress`, `agent_event`, `tool_result`,
`tool_interrupt`). This is what generic OpenAI clients see.

**AI-SDK format** is the native [Vercel AI SDK v6 UI Message Stream](https://ai-sdk.dev).
Custom UI data is carried as `data-*` parts. It also sets the
`x-vercel-ai-ui-message-stream: v1` response header. **The `agentui` frontend
uses this format** (`?format=aisdk`).

The data-part payloads (shapes for `data-artifact`, `data-task-list`,
`data-usage`, `data-agent-step`, etc.) are the source of truth for the
frontend's `ChatDataParts` type — keep them in sync with
`agentui/lib/chat/types.ts`.

See `.plans/01-stream.md` for the original cross-protocol design notes
(Claude content-block lifecycle vs Codex JSONL vs Responses API vs AI-SDK).

---

## 6. Public request/response types (`internal/types`)

```go
type ChatCompletionRequest struct {
    Model           string        // agent id OR upstream model id (router fallback)
    Messages        []ChatMessage // OpenAI {role,content} or AI-SDK {role,parts}
    Stream          *bool         // default true
    ThreadID        string        // persistence key (omit/Temporary ⇒ anon, not saved)
    UseRAG          bool          // augment messages with retrieved context
    PromptID        string        // prompt template to apply
    AgentID         string        // explicit agent selector (also accepts agentId)
    Temporary       bool          // skip persistence
    ReasoningEffort string        // model hint
}

type ResumeRequest struct {
    ThreadID string  // thread with a pending interrupt
    Action   string  // "approved" | "denied" | "skipped"
}
```

`/v1/chat/completions` non-stream returns a standard OpenAI `chat.completion`
object; stream returns SSE (§5). `/v1/models` and `/v1/agents` return
`{object:"list", data:[…]}` shaped after OpenAI, with extra fields (agent
`type`, `sub_agents`, `output_agent`, `max_iterations`, etc.).

---

## 7. Human-in-the-loop (HITL)

Some tools require approval (`internal/tools` → `HITLToolNames()`, currently
`write_database`). The flow:

1. ADK surfaces a `FunctionCall` named `adk_request_confirmation` wrapping the
   real call. The stream loop (`internal/agent/stream.go`) stores a
   `hitl.PendingInterrupt` keyed by thread id and emits a tool-interrupt frame
   (§5), then **returns** (stream pauses).

   ```go
   type PendingInterrupt struct {
       AgentID, ConfirmationCallID, ToolCallID, ToolName, Prompt string
       Details map[string]any
   }
   type Store interface { Set(threadID, *PendingInterrupt) error
                          Get(threadID) (*PendingInterrupt, error)
                          Clear(threadID) error }
   ```

2. Client calls `POST /v1/agent/resume` with `{thread_id, action}`
   (`internal/handler/resume.go`). The handler looks up the pending interrupt,
   clears it, re-surfaces the tool call, and feeds an ADK
   `adk_request_confirmation` FunctionResponse (`confirmed: true/false`) back
   into the runner — which resumes streaming the continuation.

Store backend: `HITL_STORE=memory` (default) or `valkey`.

---

## 8. Agent system

Agents are implemented in `agents/` and built on ADK primitives: `llmagent`
(single LLM), `SequentialAgent`, `ParallelAgent`, `LoopAgent`, and custom **code
agents** (Go functions yielding `session.Event`s). State flows between agents via
the ADK session state map; an agent's `output_key` exports its result for
downstream agents to read.

### Builder registry (`internal/bootstrap/bootstrap.go`)
`agentCfg.Type` → builder function:

```go
var builders = map[string]agentBuilder{
    "basic": basic.NewAgent, "deep-research": deepresearch.NewAgent,
    "triage": triage.NewAgent, "swarm": swarm.NewAgent,
    "explore": explore.NewAgent, "plan": plan.NewAgent,
    "verification": verification.NewAgent, "coordinator": coordinator.NewAgent,
    "codeguide": codeguide.NewAgent,
}
```

`BuildAgentTree(cfg, agentCfg, deps)` dispatches to the builder (default
`basic`). It's also used for per-request model overrides.

### Shared helpers (`agents/shared/shared.go`)
- `ResolveProvider(cfg, agentCfg) (baseURL, apiKey, *http.Client)` — provider +
  optional mTLS client from `models.yaml`.
- `ResolveTools(names, deps) ([]tool.Tool, error)` — tool names → ADK tools.
- `BuildLLMAgent(cfg, agentCfg, deps) (agent.Agent, error)` — assembles an
  `llmagent` (model, tools, system prompt, optional `OutputKey`, anti-tool-loop
  discipline).
- `RequireSubAgent(cfg, agentCfg, name, deps)` — resolve+build a named sub-agent
  from the parent's roster.
- `BuildSkillsManifest(osClient)` — inject `<available_skills>` from the
  OpenSearch `skills` index into a system prompt.

### `Core` (`internal/agent/core.go`)
Wraps a built agent for serving:
```go
type Core struct {
    Runner *runner.Runner; SessionManager *SessionManager; Interrupts hitl.Store
    AgentID, OutputAgent, ModelID string; SubAgentNames []string
    Config *config.Config; Logger zerolog.Logger
}
```
`OutputAgent` defaults to the last sub-agent; `ModelID` is used for
context-window lookup in usage events.

### Agent catalog
| Type | Shape | Notes |
|---|---|---|
| `basic` | single LLM + tools | default; injects skills manifest |
| `explore` | single LLM, **read-only** | strips write tools (`write_database`, memory writes, `trigger_alert`) |
| `plan` | single LLM, read-only | architect; `output_key: implementation_plan` |
| `verification` | single LLM, read-only | adversarial PASS/FAIL/PARTIAL verdict |
| `codeguide` | single LLM, read-only | usage/help agent with skills manifest |
| `triage` | Sequential → Parallel | extractor → {keyword, researcher, severity} → report |
| `deep-research` | Sequential + Loop + code agents | planner → rag → per-doc loop → gap → critic-refined report |
| `swarm` | LoopAgent | coordinator (JSON task board) → parallel dispatch → check-tasks |
| `coordinator` | LoopAgent | swarm variant with a Claude-Code-style coordinator prompt |

**Swarm/coordinator** (`agents/swarm`, `agents/coordinator`) decompose work into a
JSON **task board** in session state, run workers in parallel goroutines
(streaming events live), and loop until no pending tasks remain or
`max_iterations` is hit. State keys are namespaced, e.g.:
```go
KeyTaskBoard = "swarm:task_board"  // JSON []Task{ID,Worker,Input,Status,Result}
KeySynthesis = "swarm:synthesis"; KeyIteration = "swarm:iteration"
```
The `output_agent` worker runs **last** so the writer can read other workers'
results.

**Deep research** (`agents/deepresearch`) uses a custom `documentLoopRun` instead
of ADK's `LoopAgent` to avoid an escalation-propagation bug in adk-go v0.5.0.
State keys in `state.go` (`research_plan`, `doc_ids`, `all_findings`,
`research_gaps`, `draft_report`, `critic_feedback`, …).

### Internal/system agents (`internal/agents`)
Built separately by `BuildAll`: `compaction` (+ `_partial`, `_up_to`),
`session_memory`, `tool_summary`, `suggestion`. They use the smallest available
model and back features like context compaction and prompt suggestions.

---

## 9. Configuration

Env vars are parsed with `sethvargo/go-envconfig` into `config.Config`
(`internal/config/config.go`); YAML files are then loaded from `CONFIG_DIR`
(default `config/default`).

```go
type Config struct {
    Port int `env:"PORT,default=8000"`; Host string `env:"HOST,default=0.0.0.0"`
    AppName string `env:"APP_NAME,default=agentic"`
    ConfigDir string `env:"CONFIG_DIR,default=config/default"`
    LogLevel string; LogJSON bool
    OpenSearchURL string `env:"OPENSEARCH_URL,default=http://localhost:9200"`
    OpenSearchUsername, OpenSearchPassword string
    ConfluenceURL, ConfluencePAT string
    Valkey *pkgvalkey.Config
    HITLStore string `env:"HITL_STORE,default=memory"`     // memory|valkey
    SessionStore string `env:"SESSION_STORE,default=memory"` // memory|valkey
    Models *ModelsConfig; Agents *AgentsConfig; RAG *RAGConfig // from YAML
}
```

### `config/default/models.yaml` — providers & models
`Provider{ID, Name, BaseURL, APIKeyEnv, Models[], SSL*Env…}` supports **mTLS**
(client cert/key/CA via env-named files). `Model{ID, Name, Type(llm|embedding|
vision|agent), ContextLength, Capabilities, Architecture, …}`. Resolution helpers:
`FindProvider`, `FindProviderForModel`, `FindModel`, `ResolveModelID`
(canonicalizes `gpt-4o-mini` → `openai/gpt-4o-mini`).

### `config/default/agents.yaml` — agent roster
```yaml
- id: deep-research
  type: deep-research
  name: deep_research_pipeline
  description: …
  output_agent: report_generator
  sub_agents: [research-planner, document-analyst, …]   # references to internal agents
- id: research-planner
  internal: true            # not selectable at top level; only as a sub-agent
  output_key: research_plan # exports LLM output to session state
  model: gpt-oss-120b; provider: openrouter
  system_prompt: |
    …
```
`AgentConfig` fields: `id, type, name, description, model, provider,
system_prompt, tools[], sub_agents[], internal, output_key, output_agent,
keywords[], max_iterations, max_parallel_workers`. `WithModelOverride` /
`ResolveSubAgents` propagate per-request model overrides down the tree.

### `config/default/rag.yaml`
`RAGConfig{EmbeddingModel, TopK(=5), Index(="embeddings"), Prompt}`.

### `config/default/prompts/*.tmpl`
`text/template` files loaded by `internal/prompts` (compaction, session-memory,
tool-use-summary, prompt-suggestion).

### `config/claude-code/`
A reference set of Claude-Code-style agent definitions (`main_agent`,
`coordinator_agent`, `explore`, `plan`, `verification`, `worker`, `teammate`,
`fork`, `general_purpose`, `claude_code_guide`, `statusline_setup`, plus
`agents_index.yaml`). Use these as templates/inspiration for `agents.yaml`.

---

## 10. Persistence & retrieval

### OpenSearch (`pkg/db/opensearch`)
`EnsureIndices` creates six indices (`indices.go`):
- **embeddings** — 1024-dim `knn_vector` (cosine, HNSW) + doc metadata
  (project, doc_id, chunk_id, title, source, author, date, classification, text)
- **memories** — 1024-dim `knn_vector` scoped by (app_name, user_id)
- **prompts**, **skills** — templated content + tags + version
- **threads**, **messages** — chat persistence

Client ops: `IndexDocument`, `Get/Update/DeleteDocument`, `Search`, `KNNSearch`,
`DeleteByQuery`, `Refresh`, `Ping`.

### Valkey / Redis (`pkg/db/valkey`, `pkg/session/valkey`)
- KV helpers (`Set`/`Get` with TTL).
- ADK `session.Service` backed by Valkey: session JSON at
  `session:{app}:{user}:{sid}`, an index set `sessions:{app}:{user}`, and an
  events list `events:{app}:{user}:{sid}`. Enable with `SESSION_STORE=valkey`.

### RAG (`internal/rag`)
`EmbedQuery` calls the configured embedding model (mTLS-aware). `Client.VectorSearch`
does KNN with a **text-search fallback** if embedding fails; `GetByDocID` /
`GetSegments` reassemble chunked documents.

### Memory (`pkg/memory`)
User-scoped long-term memory in OpenSearch with vector search + text fallback,
exposed to agents as five ADK tools: `search_memories`, `add_memory`,
`update_memory`, `delete_memory`, `list_memories`.

---

## 11. Tools (`internal/tools`)

Registered via `ToolNames()` and built by `NewToolByName(name, deps)` with
injected `Deps{RAGClient, OSClient, ConfluenceClient, MemoryTools, Logger}`.
Current tools: `query_database`, `write_database` (**HITL**), `retrieve_documents`,
`opensearch_retrieve`, `web_search`, `calculate`, `query_research_db`,
`query_metrics_db`, `trigger_alert`, `classify_incident`, `get_incident_context`,
`confluence_search`, `confluence_read_page`, `view_skill`, `emit_artifact`,
`todowrite`, and the five memory tools. Tools are plain Go structs wrapped with
ADK's `functiontool.New` (JSON schema auto-generated from `desc` struct tags).

`emit_artifact` and `todowrite` get special stream handling (→ artifact / task-list
events). **Confluence** (`internal/tools/confluence`) wraps the Data-Center REST
API (CQL search + page fetch) using a PAT.

---

## 12. LLM provider abstraction (`pkg/genai`)

Both providers implement ADK's `model.LLM` (`GenerateContent` → streaming or
single `iter.Seq2[*model.LLMResponse, error]`) and convert ADK `genai.Content`
to/from provider formats:
- **`pkg/genai/anthropic`** — Anthropic SDK; repairs tool_use/tool_result
  pairing; sanitizes tool ids to `toolu_…`.
- **`pkg/genai/openai`** — OpenAI-compatible (OpenRouter, local, etc.); clamps
  tool-call ids to 40 chars with a hash map.

`internal/proxy` provides raw pass-through (`OpenUpstream`, `ForwardTo`) for the
router's "unregistered model → upstream" path and for `/v1/embeddings`.
`internal/anthropic` holds native Anthropic `/v1/messages` request/response/SSE
types for the `/v1/messages` adapter.

---

## 13. Conventions & gotchas

- **Logging**: `zerolog` everywhere; library logs are bridged via
  `internal/logbridge`. Use `LOG_JSON=true` for structured logs.
- **Output-agent gating** is central: when adding stream events, decide whether
  they belong to the main thread (`isOutputAgent(author)`) or an attributed
  sub-agent. Getting this wrong duplicates or misattributes UI content.
- **Two stream formats must stay in lockstep**: any new event type needs an impl
  in **both** `internal/stream/openai` and `internal/stream/aisdk`, and likely a
  matching `data-*` type in `agentui/lib/chat/types.ts`.
- **Task-list dedup**: boards are signed (`tasks.Signature`) and clamped
  (`tasks.Clamp`) so statuses never regress and identical snapshots aren't
  re-sent.
- **adk-go v0.5.0** has an escalation-propagation bug worked around in
  `deepresearch` — prefer custom loop code over `LoopAgent` where escalation
  must not bubble.
- **Conditional routes**: prompts/threads/skills/memories endpoints only exist
  when OpenSearch is reachable; the chat path degrades gracefully without it.
- Run `make test` before committing; `go test ./...`.
