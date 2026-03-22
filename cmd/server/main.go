package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentic/internal/agent"
	"agentic/internal/bootstrap"
	"agentic/internal/server"
)

func main() {
	ctx := context.Background()

	res, err := bootstrap.Init(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}
	cfg := res.Cfg
	logger := res.Logger

	registry := agent.NewRegistry()

	for id, rootAgent := range res.Agents {
		agentCfg := res.AgentConfigs[id]

		core, coreErr := agent.NewCoreWithAgent(cfg, agentCfg, rootAgent, res.HITLStore, res.SessionService, logger)
		if coreErr != nil {
			logger.Error().Err(coreErr).Str("agent", id).Msg("failed to create agent core, skipping")
			continue
		}

		if agentCfg.OutputAgent != "" {
			core.OutputAgent = agentCfg.OutputAgent
		} else if len(agentCfg.SubAgents) > 0 {
			core.OutputAgent = agentCfg.SubAgents[len(agentCfg.SubAgents)-1].Name
		}

		registry.Register(id, core)
		logger.Info().Str("agent", id).Msg("agent registered")
	}

	if len(registry.IDs()) == 0 {
		logger.Fatal().Msg("no agents loaded successfully")
	}

	logger.Info().Strs("agents", registry.IDs()).Msg("agent registry ready")

	router := server.NewRouter(registry, cfg, res.OSClient, logger)

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
