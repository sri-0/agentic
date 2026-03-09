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
	"agentic/internal/config"
	"agentic/internal/server"
	"agentic/internal/tools"

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

	logger.Info().
		Str("model", cfg.LLMModel).
		Str("agent_model", cfg.AgentModelName).
		Str("llm_base_url", cfg.LLMBaseURL).
		Msg("starting server")

	// Create HITL store
	hitlStore := agent.NewHITLStore()

	// Create tools
	allTools, err := tools.NewAllTools(hitlStore)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create tools")
	}
	logger.Info().Strs("tools", tools.ToolNames()).Msg("tools loaded")

	// Create agent core
	core, err := agent.NewCore(cfg, allTools, hitlStore, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create agent core")
	}

	// Build router
	router := server.NewRouter(core, logger)

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
