package agent

import (
	"context"
	"fmt"
	"net/http"

	"agentic/internal/config"
	"agentic/internal/hitl"
	pkgvalkey "agentic/pkg/db/valkey"
	sessionvalkey "agentic/pkg/session/valkey"
	genaiopenai "agentic/pkg/genai/openai"

	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
)

type Core struct {
	Runner         *runner.Runner
	SessionManager *SessionManager
	Interrupts     hitl.Store
	AgentID        string
	OutputAgent    string // name of the agent whose output goes to choices[].delta.content
	Config         *config.Config
	Logger         zerolog.Logger
}

// NewHITLStore creates a hitl.Store based on cfg.HITLStore.
// Set HITL_STORE=valkey to use Valkey/Redis. Defaults to in-memory.
func NewHITLStore(cfg *config.Config, logger zerolog.Logger) (hitl.Store, error) {
	switch cfg.HITLStore {
	case "valkey", "redis":
		if cfg.Valkey == nil {
			return nil, fmt.Errorf("HITL_STORE=%s but no Valkey config (set REDIS_HOST etc)", cfg.HITLStore)
		}
		v, err := pkgvalkey.New(context.Background(), *cfg.Valkey)
		if err != nil {
			return nil, fmt.Errorf("hitl valkey: %w", err)
		}
		logger.Info().Msg("hitl: using valkey store")
		return hitl.NewValkeyStore(v.Client, "", 0), nil
	default:
		logger.Info().Msg("hitl: using in-memory store")
		return hitl.NewInMemoryStore(), nil
	}
}

// NewSessionService creates a session.Service based on cfg.SessionStore.
// Set SESSION_STORE=valkey to use Valkey/Redis. Defaults to ADK in-memory.
func NewSessionService(cfg *config.Config, logger zerolog.Logger) (session.Service, error) {
	switch cfg.SessionStore {
	case "valkey", "redis":
		if cfg.Valkey == nil {
			return nil, fmt.Errorf("SESSION_STORE=%s but no Valkey config (set REDIS_HOST etc)", cfg.SessionStore)
		}
		v, err := pkgvalkey.New(context.Background(), *cfg.Valkey)
		if err != nil {
			return nil, fmt.Errorf("session valkey: %w", err)
		}
		logger.Info().Msg("session: using valkey store")
		return sessionvalkey.NewSessionService(v.Client, 0), nil
	default:
		logger.Info().Msg("session: using in-memory store")
		return session.InMemoryService(), nil
	}
}

func NewCore(cfg *config.Config, agentCfg *config.AgentConfig, tools []tool.Tool, interrupts hitl.Store, sessionService session.Service, logger zerolog.Logger) (*Core, error) {
	// Resolve provider config for the agent's LLM
	baseURL, apiKey, httpClient := resolveProvider(cfg, agentCfg)

	llmModel := genaiopenai.New(genaiopenai.Config{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		ModelName:  agentCfg.Model,
		HTTPClient: httpClient,
	})

	agentInstance, err := llmagent.New(llmagent.Config{
		Name:        agentCfg.Name,
		Model:       llmModel,
		Instruction: agentCfg.SystemPrompt,
		Tools:       tools,
	})
	if err != nil {
		return nil, err
	}

	sm := NewSessionManager(sessionService, cfg.AppName, logger)

	r, err := runner.New(runner.Config{
		AppName:        cfg.AppName,
		Agent:          agentInstance,
		SessionService: sessionService,
	})
	if err != nil {
		return nil, err
	}

	return &Core{
		Runner:         r,
		SessionManager: sm,
		Interrupts:     interrupts,
		AgentID:        agentCfg.ID,
		Config:         cfg,
		Logger:         logger,
	}, nil
}

// NewCoreWithAgent creates a Core using a pre-built agent hierarchy (e.g. for multi-agent setups).
func NewCoreWithAgent(cfg *config.Config, agentCfg *config.AgentConfig, rootAgent adkagent.Agent, interrupts hitl.Store, sessionService session.Service, logger zerolog.Logger) (*Core, error) {
	sm := NewSessionManager(sessionService, cfg.AppName, logger)

	r, err := runner.New(runner.Config{
		AppName:        cfg.AppName,
		Agent:          rootAgent,
		SessionService: sessionService,
	})
	if err != nil {
		return nil, err
	}

	return &Core{
		Runner:         r,
		SessionManager: sm,
		Interrupts:     interrupts,
		AgentID:        agentCfg.ID,
		Config:         cfg,
		Logger:         logger,
	}, nil
}

// resolveProvider returns the base URL, API key, and HTTP client for the agent's provider.
func resolveProvider(cfg *config.Config, agentCfg *config.AgentConfig) (baseURL, apiKey string, httpClient *http.Client) {
	if cfg.Models != nil && agentCfg.Provider != "" {
		if p := cfg.Models.FindProvider(agentCfg.Provider); p != nil {
			if p.BaseURL != "" {
				httpClient, _ = p.HTTPClient()
				return p.BaseURL, p.APIKey(), httpClient
			}
		}
	}
	return "", "", nil
}

// AgentModelIDs returns the list of model IDs that should be handled as agents
// (not proxied to upstream).
func AgentModelIDs(cfg *config.Config) []string {
	if cfg.Agents != nil {
		return cfg.Agents.AgentIDs()
	}
	return nil
}

// IsAgentModel returns true if the given model ID is an agent model.
func IsAgentModel(cfg *config.Config, modelID string) bool {
	for _, id := range AgentModelIDs(cfg) {
		if id == modelID {
			return true
		}
	}
	return false
}

// ProxyProvider returns the base URL, API key, and HTTP client for proxying
// non-agent models. It checks models.yaml providers first, then falls back to
// env config. Returns nil client only when no provider is found.
func ProxyProvider(cfg *config.Config, modelID string) (baseURL, apiKey string, client *http.Client) {
	if cfg.Models != nil {
		if p := cfg.Models.FindProviderForModel(modelID); p != nil {
			key := p.APIKey()
			if p.BaseURL != "" {
				c, _ := p.HTTPClient()
				return p.BaseURL, key, c
			}
		}
	}
	return "", "", nil
}
