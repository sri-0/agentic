// Package bootstrap provides shared initialization for all CLI entry points.
// It loads config, builds agents, and returns them ready for use by the
// production server, ADK dev UI, or interactive CLI.
package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentic/agents/coordinator"
	"agentic/agents/deepresearch"
	"agentic/agents/shared"
	"agentic/agents/swarm"
	"agentic/agents/triage"
	"agentic/internal/agent"
	internalagents "agentic/internal/agents"
	"agentic/internal/config"
	"agentic/internal/eventlog"
	"agentic/internal/hitl"
	"agentic/internal/mcp"
	internalmemory "agentic/internal/memory"
	"agentic/internal/prompts"
	"agentic/internal/roster"
	"agentic/internal/rag"
	"agentic/internal/tools"
	"agentic/internal/tools/confluence"
	"agentic/pkg/db/opensearch"
	pkgvalkey "agentic/pkg/db/valkey"
	"agentic/pkg/memory"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
)

type agentBuilder func(*config.Config, *config.AgentConfig, tools.Deps) (adkagent.Agent, error)

// builders holds the orchestrator/pipeline agent types that wire static
// sub-agent trees. Leaf LLM agents (basic, explore, plan, verification,
// codeguide) are NOT here — they all build through shared.BuildLLMAgent, with
// their former per-package behaviour expressed as declarative flags on
// AgentConfig (see applyLeafDefaults).
var builders = map[string]agentBuilder{
	"deep-research": deepresearch.NewAgent,
	"triage":        triage.NewAgent,
	"swarm":         swarm.NewAgent,
	"coordinator":   coordinator.NewAgent,
}

// leafTypes are the agent types built by the single consolidated leaf builder.
var leafTypes = map[string]bool{
	"":             true, // default
	"basic":        true,
	"explore":      true,
	"plan":         true,
	"verification": true,
	"codeguide":    true,
}

// applyLeafDefaults sets the declarative leaf-behaviour flags implied by an
// agent's type, preserving the behaviour of the former explore/plan/
// verification/codeguide packages without requiring YAML edits. Explicit YAML
// values (true) are never downgraded. Idempotent.
func applyLeafDefaults(ac *config.AgentConfig) {
	switch ac.Type {
	case "explore", "plan":
		ac.ReadOnly = true
	case "verification":
		ac.ReadOnly = true
		ac.AppendVerdict = true
	case "codeguide", "basic", "":
		// basic/default agents advertise the skills catalogue (former
		// basic.NewAgent behaviour), as does the codeguide guide agent.
		ac.InjectSkillsManifest = true
	}
}

// BuildAgentTree builds a single root agent (and its sub-agent hierarchy) for
// the given agent config. Leaf types build through the consolidated
// shared.BuildLLMAgent; orchestrator types use their registered builder. An
// empty Type defaults to a basic leaf. Exposed so callers can build trees
// per-request (e.g. with a model override).
func BuildAgentTree(cfg *config.Config, agentCfg *config.AgentConfig, deps tools.Deps) (adkagent.Agent, error) {
	agentType := agentCfg.Type
	if leafTypes[agentType] {
		applyLeafDefaults(agentCfg)
		return shared.BuildLLMAgent(cfg, agentCfg, deps)
	}
	build, ok := builders[agentType]
	if !ok {
		return nil, fmt.Errorf("unknown agent type %q", agentType)
	}
	return build(cfg, agentCfg, deps)
}

// Result holds everything produced by Init.
type Result struct {
	Cfg            *config.Config
	Logger         zerolog.Logger
	OSClient       *opensearch.Client
	MemoryService  *memory.Service
	Deps           tools.Deps
	SessionService session.Service
	HITLStore      hitl.Store
	// Agents keyed by agent config ID → built ADK agent.
	Agents map[string]adkagent.Agent
	// AgentConfigs keyed by agent config ID.
	AgentConfigs map[string]*config.AgentConfig
	// Roster is the typed registry view over the loaded agents (for the task
	// tool manifest and GET /v1/agents).
	Roster *roster.Registry
	// EventLog is the durable per-session event log (background runs + resume).
	EventLog eventlog.EventLog
	// RunCoordinator manages background, connection-decoupled agent runs.
	RunCoordinator *agent.Coordinator
	// Internal agents for system operations (compaction, session memory, suggestions, etc.)
	InternalAgents *internalagents.Registry
	// Prompt template store loaded from config/prompts/
	PromptStore *prompts.Store
	// Session memory service (structured notes in Valkey)
	SessionMemory *internalmemory.SessionMemory
	// Compaction service (context window management)
	Compaction *internalmemory.CompactionService
}

// Init loads config, connects to OpenSearch, and builds all agents from
// agents.yaml. It is the shared entry point for cmd/server, cmd/dev, and cmd/cli.
func Init(ctx context.Context) (*Result, error) {
	_ = godotenv.Load()

	cfg, err := config.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var logger zerolog.Logger
	if cfg.LogJSON {
		logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	} else {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
			With().Timestamp().Logger()
	}
	if level, err := zerolog.ParseLevel(cfg.LogLevel); err == nil {
		logger = logger.Level(level)
	}

	configDir := cfg.ConfigDir
	logger.Info().Str("config_dir", configDir).Msg("loading config files")

	modelsCfg, err := config.LoadModels(filepath.Join(configDir, "models.yaml"))
	if err != nil {
		logger.Warn().Err(err).Msg("models.yaml not found, using env config only")
	} else {
		cfg.Models = modelsCfg
		logger.Info().Int("providers", len(modelsCfg.Providers)).
			Int("models", len(modelsCfg.AllModels())).
			Msg("models config loaded")
	}

	agentsCfg, err := config.LoadAgents(filepath.Join(configDir, "agents.yaml"))
	if err != nil {
		return nil, fmt.Errorf("agents.yaml is required: %w", err)
	}
	// Merge any markdown agent definitions (config/<env>/agents/*.md), which
	// override matching YAML entries by id.
	if err := roster.LoadMarkdownDir(agentsCfg, filepath.Join(configDir, "agents")); err != nil {
		logger.Warn().Err(err).Msg("failed to load markdown agent definitions")
	}
	cfg.Agents = agentsCfg
	reg := roster.FromAgentsConfig(agentsCfg)
	logger.Info().Strs("agents", agentsCfg.AgentIDs()).
		Int("primary", len(reg.Primary())).Int("dispatchable", len(reg.Dispatchable())).
		Msg("agents config loaded")

	ragCfg, err := config.LoadRAG(filepath.Join(configDir, "rag.yaml"))
	if err != nil {
		logger.Warn().Err(err).Msg("rag.yaml not found, using defaults")
	} else {
		cfg.RAG = ragCfg
		logger.Info().Str("embedding_model", ragCfg.EmbeddingModel).Int("top_k", ragCfg.TopK).Msg("rag config loaded")
	}

	osClient := opensearch.New(opensearch.Config{
		URL:      cfg.OpenSearchURL,
		Username: cfg.OpenSearchUsername,
		Password: cfg.OpenSearchPassword,
	}, logger)

	if err := osClient.Ping(ctx); err != nil {
		logger.Warn().Err(err).Str("url", cfg.OpenSearchURL).Msg("opensearch not reachable, RAG/DB tools will degrade")
	} else {
		if err := opensearch.EnsureIndices(ctx, osClient); err != nil {
			logger.Error().Err(err).Msg("failed to ensure opensearch indices")
		} else {
			logger.Info().Str("url", cfg.OpenSearchURL).Msg("opensearch connected, indices ready")
		}
	}

	var confluenceClient *confluence.Client
	if cfg.ConfluenceURL != "" {
		confluenceClient = confluence.New(confluence.Config{
			BaseURL: cfg.ConfluenceURL,
			PAT:     cfg.ConfluencePAT,
		}, logger)
		if err := confluenceClient.Ping(ctx); err != nil {
			logger.Warn().Err(err).Str("url", cfg.ConfluenceURL).Msg("confluence not reachable, confluence tools will degrade")
		} else {
			logger.Info().Str("url", cfg.ConfluenceURL).Msg("confluence connected")
		}
	}

	ragClient := rag.NewClient(osClient, cfg)

	// Build memory toolset
	memorySvc := memory.NewService(osClient, cfg, logger)
	memToolMap := make(map[string]tool.Tool)
	memToolset, err := memory.NewToolset(memory.ToolsetConfig{Service: memorySvc})
	if err != nil {
		logger.Warn().Err(err).Msg("memory toolset unavailable")
	} else {
		for _, t := range memToolset.Tools() {
			memToolMap[t.Name()] = t
		}
		logger.Info().Int("tools", len(memToolMap)).Msg("memory toolset ready")
	}

	deps := tools.Deps{RAGClient: ragClient, OSClient: osClient, ConfluenceClient: confluenceClient, MemoryTools: memToolMap, Logger: logger}

	// MCP servers: the backend connects as an MCP client; agents that list a
	// server in mcp_servers get its tools. Missing mcp.yaml => no servers.
	mcpCfg, err := config.LoadMCP(filepath.Join(configDir, "mcp.yaml"))
	if err != nil {
		logger.Warn().Err(err).Msg("mcp.yaml failed to load")
		mcpCfg = &config.MCPConfig{}
	}
	mcpManager := mcp.NewManager(mcpCfg, logger)
	deps.MCPToolsets = mcpManager.Toolsets

	sessionService, err := agent.NewSessionService(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("session service: %w", err)
	}

	hitlStore, err := agent.NewHITLStore(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("hitl store: %w", err)
	}

	eventLog, err := agent.NewEventLog(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("event log: %w", err)
	}
	runCoordinator := agent.NewCoordinator(eventLog, logger)

	// Swarm dispatch: the task tool runs a chosen subagent (from the typed
	// registry) as a child session via its own Runner. BuildChild closes over
	// the (about-to-be-completed) deps so a dispatch-capable child also gets the
	// task tool — this is how gated nesting works (only agents whose tool list
	// includes "task" can dispatch). The closure runs lazily, after deps.TaskTool
	// is set below, so the cycle is safe.
	buildChild := func(def *roster.Definition) (adkagent.Agent, error) {
		return shared.BuildLLMAgent(cfg, def.Config(), deps)
	}
	taskTool, err := tools.NewTaskTool(tools.TaskDeps{
		Registry:       reg,
		AppName:        cfg.AppName,
		SessionService: sessionService,
		BuildChild:     buildChild,
	})
	if err != nil {
		logger.Warn().Err(err).Msg("task tool unavailable")
	} else {
		deps.TaskTool = taskTool
		logger.Info().Msg("swarm: task dispatch tool ready")
	}

	agents := make(map[string]adkagent.Agent)
	agentConfigs := make(map[string]*config.AgentConfig)

	for i := range agentsCfg.Agents {
		ac := &agentsCfg.Agents[i]

		// Internal agents are roster entries used only as resolved sub-agents.
		// They remain in cfg.Agents.Agents (so ResolveSubAgents finds them) but
		// are NOT built/registered as selectable top-level agents.
		if ac.Internal {
			continue
		}

		a, err := BuildAgentTree(cfg, ac, deps)
		if err != nil {
			logger.Error().Err(err).Str("agent", ac.ID).Str("type", ac.Type).Msg("failed to build agent, skipping")
			continue
		}

		agents[ac.ID] = a
		agentConfigs[ac.ID] = ac
		logger.Info().Str("agent", ac.ID).Str("type", ac.Type).Msg("agent built")
	}

	if len(agents) == 0 {
		return nil, fmt.Errorf("no agents loaded successfully")
	}

	// Load prompt templates
	promptStore, err := prompts.NewStore(filepath.Join(configDir, "prompts"))
	if err != nil {
		logger.Warn().Err(err).Msg("prompt templates not loaded, internal agents will be unavailable")
		promptStore = nil
	} else {
		logger.Info().Strs("templates", promptStore.Names()).Msg("prompt templates loaded")
	}

	// Build internal agents (compaction, session memory, suggestions, etc.)
	var internalReg *internalagents.Registry
	var sessionMem *internalmemory.SessionMemory
	var compactionSvc *internalmemory.CompactionService

	if promptStore != nil {
		internalReg = internalagents.BuildAll(cfg, promptStore, sessionService, logger)

		// Session memory requires Valkey
		if cfg.Valkey != nil {
			valkeyInstance, err := pkgvalkey.New(ctx, *cfg.Valkey)
			if err != nil {
				logger.Warn().Err(err).Msg("valkey not available for session memory")
			} else {
				sessionMem = internalmemory.NewSessionMemory(valkeyInstance, promptStore, internalReg, sessionService, logger)
				logger.Info().Msg("session memory service ready")
			}
		}

		compactionSvc = internalmemory.NewCompactionService(promptStore, internalReg, sessionService, logger)
		logger.Info().Msg("compaction service ready")
	}

	return &Result{
		Cfg:            cfg,
		Logger:         logger,
		OSClient:       osClient,
		MemoryService:  memorySvc,
		Deps:           deps,
		SessionService: sessionService,
		HITLStore:      hitlStore,
		Agents:         agents,
		AgentConfigs:   agentConfigs,
		Roster:         reg,
		EventLog:       eventLog,
		RunCoordinator: runCoordinator,
		InternalAgents: internalReg,
		PromptStore:    promptStore,
		SessionMemory:  sessionMem,
		Compaction:     compactionSvc,
	}, nil
}
