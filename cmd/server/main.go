package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"agentic/agents/deepresearch"
	"agentic/agents/triage"
	"agentic/internal/agent"
	"agentic/internal/config"
	"agentic/internal/rag"
	"agentic/internal/server"
	"agentic/internal/tools"

	adkagent "google.golang.org/adk/agent"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
)

func main() {
	ctx := context.Background()

	// Load .env file if present
	_ = godotenv.Load()

	// Load configuration
	cfg, err := config.Load(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Init logger
	var logger zerolog.Logger
	if cfg.LogJSON {
		logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	} else {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
			With().Timestamp().Logger()
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err == nil {
		logger = logger.Level(level)
	}

	// Load YAML configs from CONFIG_DIR
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
		logger.Fatal().Err(err).Msg("agents.yaml is required")
	}
	cfg.Agents = agentsCfg
	logger.Info().Strs("agents", agentsCfg.AgentIDs()).Msg("agents config loaded")

	// Build all agents into registry
	ragClient := rag.NewClient()
	registry := agent.NewRegistry()

	for i := range agentsCfg.Agents {
		agentCfg := &agentsCfg.Agents[i]

		var core *agent.Core

		if len(agentCfg.SubAgents) > 0 {
			var rootAgent adkagent.Agent
			var buildErr error

			switch agentCfg.ID {
			case "triage-agent":
				rootAgent, buildErr = triage.NewAgent(cfg, agentCfg, ragClient)
			default:
				// deep-research and any other hierarchical agents
				rootAgent, buildErr = deepresearch.NewAgent(cfg, agentCfg, ragClient)
			}

			if buildErr != nil {
				logger.Fatal().Err(buildErr).Str("agent", agentCfg.ID).Msg("failed to create hierarchical agent")
			}

			core, err = agent.NewCoreWithAgent(cfg, agentCfg, rootAgent, logger)
			if err != nil {
				logger.Fatal().Err(err).Str("agent", agentCfg.ID).Msg("failed to create agent core")
			}
			logger.Info().Str("agent", agentCfg.ID).Int("sub_agents", len(agentCfg.SubAgents)).Msg("hierarchical agent loaded")
		} else {
			allTools, toolErr := tools.NewAllTools()
			if toolErr != nil {
				logger.Fatal().Err(toolErr).Str("agent", agentCfg.ID).Msg("failed to create tools")
			}
			core, err = agent.NewCore(cfg, agentCfg, allTools, logger)
			if err != nil {
				logger.Fatal().Err(err).Str("agent", agentCfg.ID).Msg("failed to create agent core")
			}
			logger.Info().Str("agent", agentCfg.ID).Strs("tools", tools.ToolNames()).Msg("flat agent loaded")
		}

		registry.Register(agentCfg.ID, core)
	}

	logger.Info().Strs("agents", registry.IDs()).Msg("agent registry ready")

	// Build router
	router := server.NewRouter(registry, cfg, logger)

	// Start server
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // long timeout for SSE streams
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info().Str("signal", sig.String()).Msg("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("shutdown error")
		}
	}()

	logger.Info().Str("addr", addr).Msg("server listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal().Err(err).Msg("server error")
	}
}
