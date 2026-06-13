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

	"agentic/agents/basic"
	"agentic/agents/codeguide"
	"agentic/agents/coordinator"
	"agentic/agents/deepresearch"
	"agentic/agents/explore"
	"agentic/agents/plan"
	"agentic/agents/swarm"
	"agentic/agents/triage"
	"agentic/agents/verification"
	"agentic/internal/agent"
	internalagents "agentic/internal/agents"
	"agentic/internal/config"
	"agentic/internal/hitl"
	internalmemory "agentic/internal/memory"
	"agentic/internal/prompts"
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

var builders = map[string]agentBuilder{
	"basic":         basic.NewAgent,
	"deep-research": deepresearch.NewAgent,
	"triage":        triage.NewAgent,
	"swarm":         swarm.NewAgent,
	"explore":       explore.NewAgent,
	"plan":          plan.NewAgent,
	"verification":  verification.NewAgent,
	"coordinator":   coordinator.NewAgent,
	"codeguide":     codeguide.NewAgent,
}

// BuildAgentTree builds a single root agent (and its sub-agent hierarchy) for
// the given agent config, using the registered builder for its type. An empty
// Type defaults to "basic". This is the same logic the startup loop uses, but
// exposed so callers can build agent trees per-request (e.g. with a model
// override).
func BuildAgentTree(cfg *config.Config, agentCfg *config.AgentConfig, deps tools.Deps) (adkagent.Agent, error) {
	agentType := agentCfg.Type
	if agentType == "" {
		agentType = "basic"
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
	cfg.Agents = agentsCfg
	logger.Info().Strs("agents", agentsCfg.AgentIDs()).Msg("agents config loaded")

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

	sessionService, err := agent.NewSessionService(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("session service: %w", err)
	}

	hitlStore, err := agent.NewHITLStore(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("hitl store: %w", err)
	}

	agents := make(map[string]adkagent.Agent)
	agentConfigs := make(map[string]*config.AgentConfig)

	for i := range agentsCfg.Agents {
		ac := &agentsCfg.Agents[i]

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
		InternalAgents: internalReg,
		PromptStore:    promptStore,
		SessionMemory:  sessionMem,
		Compaction:     compactionSvc,
	}, nil
}
