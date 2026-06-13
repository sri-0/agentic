package agents

import (
	"fmt"
	"net/http"

	"agentic/internal/config"
	"agentic/internal/prompts"
	genaiopenai "agentic/pkg/genai/openai"

	"github.com/rs/zerolog"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
)

// BuildAll creates all internal agents and returns a populated registry.
func BuildAll(cfg *config.Config, promptStore *prompts.Store, sessionSvc session.Service, logger zerolog.Logger) *Registry {
	reg := NewRegistry()

	// Resolve model provider for internal agents — use the first configured provider
	baseURL, apiKey, httpClient := resolveInternalProvider(cfg)
	if baseURL == "" {
		logger.Warn().Msg("no provider found for internal agents, skipping")
		return reg
	}

	// Use a smaller/faster model for internal operations if available
	modelName := resolveInternalModel(cfg)

	builders := []struct {
		name        string
		instruction string
	}{
		{
			name:        "compaction",
			instruction: promptStore.MustRender("compaction_full", prompts.CompactionData{}),
		},
		{
			name:        "compaction_partial",
			instruction: promptStore.MustRender("compaction_partial", prompts.CompactionData{}),
		},
		{
			name:        "compaction_up_to",
			instruction: promptStore.MustRender("compaction_up_to", prompts.CompactionData{}),
		},
		{
			name:        "session_memory",
			instruction: "You are a session memory extraction agent. You will receive conversation messages and current session notes, then output the complete updated notes document.",
		},
		{
			name:        "tool_summary",
			instruction: promptStore.MustRender("tool_use_summary", nil),
		},
		{
			name:        "suggestion",
			instruction: promptStore.MustRender("prompt_suggestion", nil),
		},
	}

	for _, b := range builders {
		m := genaiopenai.New(genaiopenai.Config{
			APIKey:     apiKey,
			BaseURL:    baseURL,
			ModelName:  modelName,
			HTTPClient: httpClient,
		})

		a, err := llmagent.New(llmagent.Config{
			Name:        b.name,
			Description: fmt.Sprintf("Internal agent: %s", b.name),
			Model:       m,
			Instruction: b.instruction,
		})
		if err != nil {
			logger.Error().Err(err).Str("agent", b.name).Msg("failed to build internal agent")
			continue
		}

		r, err := runner.New(runner.Config{
			Agent:          a,
			AppName:        "agentic-internal",
			SessionService: sessionSvc,
		})
		if err != nil {
			logger.Error().Err(err).Str("agent", b.name).Msg("failed to create runner for internal agent")
			continue
		}

		reg.Register(b.name, &InternalAgent{
			Agent:  a,
			Runner: r,
			Name:   b.name,
		})
		logger.Info().Str("agent", b.name).Msg("internal agent built")
	}

	return reg
}

func resolveInternalProvider(cfg *config.Config) (string, string, *http.Client) {
	if cfg.Models == nil {
		return "", "", nil
	}
	for _, p := range cfg.Models.Providers {
		if p.BaseURL != "" && p.APIKey() != "" {
			httpClient, _ := p.HTTPClient()
			return p.BaseURL, p.APIKey(), httpClient
		}
	}
	return "", "", nil
}

func resolveInternalModel(cfg *config.Config) string {
	// Prefer a fast/cheap model for internal operations
	// Look for models with "gpt-4o-mini" or similar in their ID
	if cfg.Models != nil {
		for _, m := range cfg.Models.AllModels() {
			if m.ID == "openai/gpt-4o-mini" || m.ID == "gpt-4o-mini" {
				return m.ID
			}
		}
		// Fallback to first available model
		models := cfg.Models.AllModels()
		if len(models) > 0 {
			return models[0].ID
		}
	}
	return "gpt-oss-120b"
}
