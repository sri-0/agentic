The three layers (this is the key mental model)

opencode separates three things your current design smushes together:

1. The agent definition (the TYPE) — static, typed, registered ahead of time

An "agent" is just a config object. From packages/opencode/src/agent/agent.ts:

Agent {
  name:        string          // "researcher", "explore", "writer", "gap-analyst"
  description: string          // shown to BOTH the coordinator AND your UI
  mode:        "primary" | "subagent" | "all"
  model?:      { providerID, modelID }   // own model, or inherit parent's
  prompt?:     string          // the specialist system prompt
  permission:  Ruleset         // which tools it's allowed (glob, last-match-wins)
  steps?:      number          // max agentic iterations (budget)
  temperature?, topP?, options
}

These are defined two ways: built-ins in code (build, plan, general, explore, plus hidden compaction/title/summary), and user agents as markdown files with YAML frontmatter (frontmatter → fields, markdown body → prompt). All three sources (code + markdown + JSON overrides) merge into one registry at boot.

This registry is your typed roster. It's fully known before any run starts. mode: subagent = only spawnable by the coordinator; primary = user talks to it directly; all = both.

2. The task tool (the DYNAMIC dispatch)

The coordinator is not a special orchestrator type. It's just an ordinary LLM agent that has one extra tool: task. From packages/opencode/src/tool/task.ts, its parameters are:

task({
  subagent_type: string   // MUST match a registered Agent.name (mode subagent|all)
  description:   string    // short label → becomes the child session title
  prompt:        string    // the actual task, composed by the coordinator AT RUNTIME
  background?:   boolean    // run async, don't block the coordinator
  task_id?:      string     // resume a previously-spawned subagent
})

The coordinator's system prompt enumerates the available subagent_types and their descriptions (pulled straight from the registry). So at runtime the LLM dynamically decides: which type to spawn, how many instances, what task prompt to give each, whether to run them in parallel or sequence, and whether to background them. That's your dynamic spawning and selection — but every spawn references a known, typed name. The coordinator can't invent a new agent with arbitrary tools; it picks from the curated catalog and supplies a fresh task.

3. The child session (the runtime INSTANCE)

This is the part that makes it clean. Each task() call creates a real child session with parentID = the coordinator's session id:

nextSession = sessions.create({
  parentID:   ctx.sessionID,                       // links it to the coordinator
  title:      params.description + " (@researcher subagent)",
  agent:      params.subagent_type,                // the TYPE it's an instance of
  permission: [...derivedFromParent, ...subagentRules],
})

The subagent then runs through the exact same prompt/LLM pipeline as a top-level agent — its own history, its own tool set (filtered by its permission ruleset), its own model (its own, or inherited). When it finishes, its final text is returned to the coordinator as a tool result wrapped in <task_result>…</task_result>. The coordinator reads that and decides what to do next (spawn more, synthesize, finish).

By default a subagent cannot spawn its own subagents (the task tool is denied to it) — that's a deliberate guardrail to keep the tree shallow. You can opt specific agents into nesting.

How this gives you the typed structure for your UI

This is the part you're worried about, and it falls out naturally — because every running thing is an instance of a known type:

- The catalog is typed and known upfront. Your GET /v1/agents already returns the roster (agentui already calls it). So your UI knows every subagent type — name, description, icon — before anything runs.
- Every spawn is a typed reference. A child session carries agent: "researcher" + a unique id + parentID. So when the UI sees a subagent come alive, it knows exactly which typed card to render and how to label it. Your agentui already does this — AgentCards keys cards by agentId cross-referenced against /v1/agents.
- The coordinator's plan is itself typed. "Spawn researcher ×3, then gap-analyst, then writer" maps directly onto your existing data-task-list part (each task has {id, title, status, agent}) — the agent field on each task is the typed link to the worker card. Your TaskBar already renders exactly this.
- The streamed work is typed. Each subagent's tokens flow as data-agent-delta {agent, kind, delta}, step logs as data-agent-progress, attributed to that child session. Your agent-view.tsx side panel already consumes these.

So the wire contract your frontend already speaks (data-task-list, data-agent-delta, data-agent-step, agent cards keyed by type) is exactly the typed structure that an opencode-style swarm produces. The reason it feels awkward today is that your backend manufactures it by parsing a stringly-typed JSON task board out of ADK session state (coordinator/dispatch.go re-authors worker events as worker#taskID), instead of it being a first-class child-session model.

Side-by-side with what you have now

┌────────────────────────┬─────────────────────────────────────────────────────────────────────────────────────────────────────┬───────────────────────────────────────────────────────────────────────────┐
│                        │                                           Current agentic                                           │                              opencode model                               │
├────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────┤
│ Coordinator            │ Special LoopAgent type                                                                              │ Ordinary LLM agent + a task tool                                          │
├────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────┤
│ How work is assigned   │ LLM emits a full JSON task board into ADK state every turn; dispatch.go parses & runs fixed workers │ LLM calls task(type, prompt) per subagent; backend spawns a child session │
├────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────┤
│ Worker roster          │ The agent's static sub_agents: list in YAML                                                         │ A typed registry (code + markdown), advertised to coordinator AND UI      │
├────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────┤
│ Is it dynamic?         │ Partially — fixed workers, dynamic task text                                                        │ Yes — dynamic type-selection, count, prompt, parallelism, background      │
├────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────┤
│ Worker identity for UI │ Synthesized by re-authoring events worker#taskID + parsing the board                                │ Native: child session with {id, parentID, agent: type}                    │
├────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────┤
│ Result passing         │ Strings on the board in session state                                                               │ <task_result> tool-result back to coordinator                             │
├────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────┤
│ Nesting                │ N/A (flat)                                                                                          │ Supported, gated per-agent (default off)                                  │
└────────────────────────┴─────────────────────────────────────────────────────────────────────────────────────────────────────┴───────────────────────────────────────────────────────────────────────────┘

The net: you get dynamic spawning/selection and a typed structure, because "dynamic" applies to the dispatch decisions while "typed" applies to the catalog and the per-instance identity. Nothing is stringly-typed; the coordinator just picks names from a registry and hands each a task, and each running instance is a typed, addressable child session that your UI already knows how to render.

One thing worth flagging for your case: since you're a gateway (OpenAI-compatible, multi-client), the registry-driven approach is especially nice — the same /v1/agents catalog drives the coordinator's tool description, the UI's cards, and validation that a requested subagent_type actually exists.