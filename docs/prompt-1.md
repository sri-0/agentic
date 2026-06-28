i have two codebases ~/code/agentic (this work dir) whcih is my agentic LLM gateway, its a golang backend API using adk-go. And my frontend is ~/code/agentui.

I want you to analyze the ~/code/opencode repo for the following features and analyse my current code base ~/code/agentic in particular the agent/subagent architecutre, and come up with a simplified and improved approach (through a comprehensive plan, use seperate details plans store in ./plans for each phase) for agent swarms that is modelled off opencode/claude code architecture. Research adk-go. Use sub-agents to do the research and to write the subplans. Grill me with heaps of questions until you understand what my design preferences are here. Take as much time as possible to ensure you know everything & find the right context.

* Agent swarm design
    - agent/subagent branch out - i want to have a set of sub agents that the swarm coordinator can use/assign
        - eg. data researcher/analyst, gap analyst, report writer, explore, plan etc
        - each sub agent has specific prompts and tools available to it.
        - eg. deep research swarm would be possible through this patter
    - coordinator
* task list/todo list agent
    - managing task state in redis?
* internal agents (eg. not used for subagents, but are used internally)
    - compaction
    - memory
    - summariser (eg. 1 line recap/thread titles)
    - auto suggestions
    - any others? 
- "auto mode" agent
    - this agent id routes to an agent which is a classifier
    - it determines whether to use more complex agents, such as deep research agent or swarm before answering the question
    - confirm if this is how opencode/claude code does it, since sometimes their clis return simple responses eg. hi, this must not be using an agent under the hood.
    - the behaviour we want is, if complex task use agents, if a simple hello pass through directly to LLM. 
    - the auto agent acts as a routing layer.
- memory
    - short term thread memory - redis.
    - long term memory opensearch
- server-side sessions (design & plan out only)
    - How can we design the agents so that they still run even if browser disconnects.
    - continue to run the agents in background, then come back to the UI later and see sessions which are still running
    - store current sessions in redis, along with the status of each agent. 
    - Then potentially stream the openai sse outputs to kakfka, to allow users to resume a stream anytime?? Or redis?
- mcp integration
    - how would we go about planning out MCP integration, agents have access to MCPs
    - eg. design how mcp auth would work using the gitlab mcp api as an example:
        - do we do a client side redirect to the gitlab login, then keep the token client side and pass it through headers to our backend to use?
        - the backend needs to be talking to the MCP server, not the frontend.
- Question agents (interview the user)
    - ask the users questions, this then gets rendered in our UI
    - which protocol would need to be used for this? or is it just structured LLM outputs?
- Office document agents/tools
    - eg. how we could have tools to write pptx, docx files
    - could use for report writing
    - execution environent - how would this execute? golang? or do need a python sandbox.