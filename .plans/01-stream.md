 There are three distinct approaches in production:

  Anthropic (Claude Code)

  Uses SSE with a hierarchical content block lifecycle:

  message_start
    content_block_start  (type: "text" | "tool_use" | "thinking")
      content_block_delta  (type: "text_delta" | "input_json_delta" | "thinking_delta")
      content_block_delta  ...
    content_block_stop
    content_block_start  (next block)
      ...
    content_block_stop
  message_delta  (stop_reason)
  message_stop

  Tool inputs stream as partial JSON strings (input_json_delta). Tool results come back in the next request turn as tool_result content blocks. Thinking/reasoning streams as thinking_delta. Each content block has an index for ordering.

  Claude Code's CLI (--output-format stream-json) wraps these in a higher-level envelope with types: system, stream_event (containing the raw API events above), assistant, and result.

  OpenAI Codex CLI

  Uses JSONL over stdio (not SSE). JSON-RPC-lite messages:

  {"type":"thread.started","thread_id":"..."}
  {"type":"turn.started"}
  {"type":"item.started","item":{"type":"command_execution","command":"ls","status":"in_progress"}}
  {"type":"item.completed","item":{"type":"command_execution","output":"README.md\nsrc/"}}
  {"type":"item.started","item":{"type":"agent_message","text":""}}
  {"type":"item.completed","item":{"type":"agent_message","text":"The repo has..."}}
  {"type":"turn.completed"}

  Item types: agent_message, command_execution, file_change, mcp_call, reasoning, plan_update, approval_request, diff. Each item has a start/delta/completed lifecycle.

  OpenAI Responses API

  Uses SSE with hierarchical dot-separated event names:

  event: response.created
  event: response.output_item.added       (type: "function_call" | "text" | ...)
  event: response.function_call_arguments.delta   (partial JSON)
  event: response.function_call_arguments.done
  event: response.output_item.done
  event: response.output_text.delta       (text token)
  event: response.output_text.done
  event: response.completed

  Every event carries a sequence_number for ordering.

  Vercel AI SDK (what your Zola UI likely uses)

  Uses SSE with typed JSON data lines:

  data: {"type":"message-start","id":"msg_..."}
  data: {"type":"text-delta","delta":"Hello"}
  data: {"type":"tool-input-start","toolCallId":"call_...","toolName":"get_weather"}
  data: {"type":"tool-input-delta","toolCallId":"call_...","inputTextDelta":"{\"city\":"}
  data: {"type":"tool-input-available","toolCallId":"call_...","input":{"city":"London"}}
  data: {"type":"tool-result","toolCallId":"call_...","output":{...}}
  data: {"type":"reasoning-delta","delta":"Let me think..."}
  data: [DONE]

  ---
  Key Pattern Across All: Item Lifecycle

  Every system uses the same core abstraction — items with a start → delta → done lifecycle*:

  ┌──────────────┬──────────────────────────────────────────────────────────────┬────────────────────────────────────┬─────────────────────────────────────────────────────────────────┬───────────────────────────────────────────────────┐
  │   Concept    │                            Claude                            │               Codex                │                          Responses API                          │                   Vercel AI SDK                   │
  ├──────────────┼──────────────────────────────────────────────────────────────┼────────────────────────────────────┼─────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────┤
  │ Text         │ content_block_start → text_delta* → content_block_stop       │ item.started → item.completed      │ output_item.added → output_text.delta* → output_text.done       │ text-delta* → text-end                            │
  │ streaming    │                                                              │                                    │                                                                 │                                                   │
  ├──────────────┼──────────────────────────────────────────────────────────────┼────────────────────────────────────┼─────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────┤
  │ Tool call    │ content_block_start(tool_use) → input_json_delta* →          │ item.started(mcp_call) →           │ output_item.added(function_call) → fn_call_args.delta* →        │ tool-input-start → tool-input-delta* →            │
  │              │ content_block_stop                                           │ item.completed                     │ fn_call_args.done                                               │ tool-input-available                              │
  ├──────────────┼──────────────────────────────────────────────────────────────┼────────────────────────────────────┼─────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────┤
  │ Tool result  │ Next turn tool_result block                                  │ item.completed with output         │ Submit output, new response                                     │ tool-result event                                 │
  ├──────────────┼──────────────────────────────────────────────────────────────┼────────────────────────────────────┼─────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────┤
  │ Thinking     │ thinking_delta*                                              │ reasoning item                     │ N/A                                                             │ reasoning-delta*                                  │
  └──────────────┴──────────────────────────────────────────────────────────────┴────────────────────────────────────┴─────────────────────────────────────────────────────────────────┴───────────────────────────────────────────────────┘

  ---
  What We Should Implement

  Our current SSE format is OpenAI chat-completions compatible (choices[0].delta.content) with custom extension events (agent_progress, agent_event, tool_result, tool_interrupt). This is fine for the Vercel AI SDK / Zola UI.

  But the current custom events are flat — they don't have the item lifecycle pattern. Here's what I'd propose aligning to:

  Proposed Event Protocol

  Keep OpenAI chat-completions as the base (for final output), but formalize the agent events with proper lifecycles:

  // Agent lifecycle (maps to prompt-kit Steps component)
  data: {"agent_progress":{"phase":"agent_start","agent":"research_planner","step":1,"message":"Planning research..."}}
  data: {"agent_progress":{"phase":"agent_done","agent":"research_planner","step":1,"message":"Plan ready"}}

  // Agent text streaming (maps to a streaming markdown block under each step)
  data: {"agent_event":{"agent":"document_analyst","step":3,"type":"text_delta","content":"The document shows..."}}
  data: {"agent_event":{"agent":"document_analyst","step":3,"type":"text_done"}}

  // Tool lifecycle (within an agent's step)
  data: {"agent_event":{"agent":"data_analyst","step":3,"type":"tool_start","tool_name":"query_research_db","tool_call_id":"call_1","args":{"sql":"..."}}}
  data: {"agent_event":{"agent":"data_analyst","step":3,"type":"tool_done","tool_name":"query_research_db","tool_call_id":"call_1","result":{...}}}

  // Final output agent streams to standard OpenAI format
  data: {"choices":[{"delta":{"content":"# Research Report\n\n..."}}]}

  // HITL interrupt (unchanged)
  data: {"tool_interrupt":{"toolCallId":"...","toolName":"write_database","prompt":"Approve?"}}

  data: [DONE]

  The UI maps this as:

  ┌─ Step 1: research_planner ✓
  │  "Identified 3 search queries and 4 focus areas"
  ├─ Step 2: rag_retrieval ✓
  │  "Retrieved 5 documents"
  ├─ Step 3: document_loop (iteration 1/5)
  │  ├─ document_analyst: "The document shows revenue growth of..."
  │  ├─ data_analyst: [tool: query_research_db] → 12 rows
  │  └─ findings_synthesizer: "Key finding: revenue increased 23%..."
  ├─ Step 3: document_loop (iteration 2/5)
  │  ...
  ├─ Step 4: gap_analyst ✓
  │  "Missing: competitive analysis data"
  ├─ Step 5: report_generator
  │  ▼ streaming final report...
  │
  │  # Research Report
  │  ## Executive Summary
  │  ...

  What Changes

  1. Add tool_start/tool_done to agent_event — currently tool calls go to choices[0].delta.tool_calls which is the OpenAI format. For intermediate agents, tool calls should route to agent_event instead, just like text does.
  2. Add iteration tracking — the agent_progress for loop agents should include iteration info ("iteration": 2, "max_iterations": 5).
  3. The stream.go routing logic already handles the agent/output split. We just need to extend it with tool lifecycle events for non-output agents.

  ---
  Codebase Structure (Revised)

  agents/
    shared/
      shared.go              # ResolveProvider, ResolveTools, BuildLLMAgent, RequireSubAgent

    basic/
      agent.go               # flat LLM agent

    deepresearch/
      coordinator.go         # NewAgent() — root LLM coordinator + AgentTool wiring
      planner.go             # research_planner LLM agent
      pipeline.go            # research_pipeline Sequential (rag → loop → gap → report)
      critic.go              # report_critic LLM agent
      loop.go                # document_loop code agents (fetcher, accumulator)
      state.go               # state helpers + key constants

    triage/
      coordinator.go         # NewAgent() — root LLM coordinator
      pipeline.go            # triage_pipeline Sequential

  Want me to implement this? I'd do it in this order:

  1. Add tool_start/tool_done to the agent_event type and update stream.go routing
  2. Restructure deep research into coordinator + planner + pipeline + critic
  3. Update agents.yaml with the new sub-agent configs
  4. Update tests
  5. Do the same for triage