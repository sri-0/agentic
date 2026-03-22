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
	"agentic/agents/deepresearch"
	"agentic/agents/triage"
	"agentic/internal/agent"
	"agentic/internal/config"
	"agentic/internal/hitl"
	"agentic/internal/rag"
	"agentic/internal/tools"
	"agentic/pkg/db/opensearch"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
)

type agentBuilder func(*config.Config, *config.AgentConfig, tools.Deps) (adkagent.Agent, error)

var builders = map[string]agentBuilder{
	"basic":         basic.NewAgent,
	"deep-research": deepresearch.NewAgent,
	"triage":        triage.NewAgent,
}

// Result holds everything produced by Init.
type Result struct {
	Cfg            *config.Config
	Logger         zerolog.Logger
	OSClient       *opensearch.Client
	Deps           tools.Deps
	SessionService session.Service
	HITLStore      hitl.Store
	// Agents keyed by agent config ID → built ADK agent.
	Agents map[string]adkagent.Agent
	// AgentConfigs keyed by agent config ID.
	AgentConfigs map[string]*config.AgentConfig
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

	ragClient := rag.NewClient(osClient, cfg)
	deps := tools.Deps{RAGClient: ragClient, OSClient: osClient}

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
		agentType := ac.Type
		if agentType == "" {
			agentType = "basic"
		}

		build, ok := builders[agentType]
		if !ok {
			logger.Error().Str("agent", ac.ID).Str("type", agentType).Msg("unknown agent type, skipping")
			continue
		}

		a, err := build(cfg, ac, deps)
		if err != nil {
			logger.Error().Err(err).Str("agent", ac.ID).Msg("failed to build agent, skipping")
			continue
		}

		agents[ac.ID] = a
		agentConfigs[ac.ID] = ac
		logger.Info().Str("agent", ac.ID).Str("type", agentType).Msg("agent built")
	}

	if len(agents) == 0 {
		return nil, fmt.Errorf("no agents loaded successfully")
	}

	return &Result{
		Cfg:            cfg,
		Logger:         logger,
		OSClient:       osClient,
		Deps:           deps,
		SessionService: sessionService,
		HITLStore:      hitlStore,
		Agents:         agents,
		AgentConfigs:   agentConfigs,
	}, nil
}
