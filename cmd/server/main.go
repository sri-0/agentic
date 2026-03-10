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

	"agentic/agents/basic"
	"agentic/agents/deepresearch"
	"agentic/agents/triage"
	"agentic/internal/agent"
	"agentic/internal/config"
	"agentic/internal/rag"
	"agentic/internal/server"

	adkagent "google.golang.org/adk/agent"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
)

type agentBuilder func(*config.Config, *config.AgentConfig, *rag.Client) (adkagent.Agent, error)

var builders = map[string]agentBuilder{
	"basic":         basic.NewAgent,
	"deep-research": deepresearch.NewAgent,
	"triage":        triage.NewAgent,
}

func main() {
	ctx := context.Background()

	_ = godotenv.Load()

	cfg, err := config.Load(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

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

	ragClient := rag.NewClient()
	registry := agent.NewRegistry()

	for i := range agentsCfg.Agents {
		agentCfg := &agentsCfg.Agents[i]
		agentType := agentCfg.Type
		if agentType == "" {
			agentType = "basic"
		}

		build, ok := builders[agentType]
		if !ok {
			logger.Error().Str("agent", agentCfg.ID).Str("type", agentType).Msg("unknown agent type, skipping")
			continue
		}

		rootAgent, buildErr := build(cfg, agentCfg, ragClient)
		if buildErr != nil {
			logger.Error().Err(buildErr).Str("agent", agentCfg.ID).Msg("failed to build agent, skipping")
			continue
		}

		core, coreErr := agent.NewCoreWithAgent(cfg, agentCfg, rootAgent, logger)
		if coreErr != nil {
			logger.Error().Err(coreErr).Str("agent", agentCfg.ID).Msg("failed to create agent core, skipping")
			continue
		}

		// Set OutputAgent — the agent whose text goes to choices[0].delta.content.
		// Explicit config takes precedence; otherwise fall back to last sub-agent.
		if agentCfg.OutputAgent != "" {
			core.OutputAgent = agentCfg.OutputAgent
		} else if len(agentCfg.SubAgents) > 0 {
			core.OutputAgent = agentCfg.SubAgents[len(agentCfg.SubAgents)-1].Name
		}

		registry.Register(agentCfg.ID, core)
		logger.Info().Str("agent", agentCfg.ID).Str("type", agentType).Msg("agent loaded")
	}

	if len(registry.IDs()) == 0 {
		logger.Fatal().Msg("no agents loaded successfully")
	}

	logger.Info().Strs("agents", registry.IDs()).Msg("agent registry ready")

	router := server.NewRouter(registry, cfg, logger)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

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
