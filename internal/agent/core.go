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
	ModelID        string // resolved model id of the (output) agent, for context_length lookup
	SubAgentNames  []string // names of sub-agents (for multi-agent task-list snapshots); empty => single-agent run
	Config         *config.Config
	Logger         zerolog.Logger
}

// OverrideCoreFunc builds a per-request *Core for the given agent config with
// the agent's (and all sub-agents') model overridden to modelID/provider. It is
// supplied by the server wiring so the chat handler can rebuild an agent tree on
// demand. It may be nil, in which case no override is performed.
type OverrideCoreFunc func(agentCfg *config.AgentConfig, modelID, provider string) (*Core, error)

// ConfigureCore sets the OutputAgent, SubAgentNames, and ModelID fields on a
// Core from its AgentConfig. Shared by the startup registration loop and the
// per-request override builder so both produce identically configured cores.
//
// ModelID is resolved for context_window lookup: prefer the output agent's
// model when it maps to a known sub-agent, else the root model. When the config
// has been built with a model override, every sub-agent carries the override
// model, so ModelID ends up as the override model id (which is what we want).
func ConfigureCore(core *Core, agentsCfg *config.AgentsConfig, agentCfg *config.AgentConfig) {
	// Resolve sub-agent configs from the flat roster (may be empty / error for
	// single-agent configs — in that case resolved is nil and we degrade).
	var resolved []*config.AgentConfig
	if agentsCfg != nil {
		resolved, _ = agentsCfg.ResolveSubAgents(agentCfg)
	}

	if agentCfg.OutputAgent != "" {
		core.OutputAgent = agentCfg.OutputAgent
	} else if len(resolved) > 0 {
		core.OutputAgent = resolved[len(resolved)-1].Name
	}

	// Record sub-agent names (multi-agent runs publish a task-list snapshot).
	core.SubAgentNames = nil
	for _, sa := range resolved {
		core.SubAgentNames = append(core.SubAgentNames, sa.Name)
	}

	core.ModelID = agentCfg.Model
	if agentsCfg != nil {
		if sa, err := agentsCfg.ResolveSubAgentByName(agentCfg, core.OutputAgent); err == nil && sa.Model != "" {
			core.ModelID = sa.Model
		}
	}
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
