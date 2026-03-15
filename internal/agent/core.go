package agent

import (
	"context"
	"net/http"

	"agentic/internal/config"
	genaiopenai "agentic/pkg/genai/openai"

	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
)

// PendingInterrupt stores the minimal info needed to map a thread to its
// ADK confirmation call ID so the resume endpoint can construct the
// FunctionResponse to send back to the runner.
type PendingInterrupt struct {
	ConfirmationCallID string         // the adk_request_confirmation function call ID
	ToolCallID         string         // the original tool's function call ID
	ToolName           string         // e.g. "write_database"
	Prompt             string         // human-readable hint
	Details            map[string]any // original tool args for display
}

// InterruptStore is a thin thread-safe map from threadID → PendingInterrupt.
type InterruptStore struct {
	mu      chan struct{} // simple mutex via buffered channel
	pending map[string]*PendingInterrupt
}

func NewInterruptStore() *InterruptStore {
	s := &InterruptStore{
		mu:      make(chan struct{}, 1),
		pending: make(map[string]*PendingInterrupt),
	}
	s.mu <- struct{}{} // initialize unlocked
	return s
}

func (s *InterruptStore) Set(threadID string, p *PendingInterrupt) {
	<-s.mu
	s.pending[threadID] = p
	s.mu <- struct{}{}
}

func (s *InterruptStore) Get(threadID string) *PendingInterrupt {
	<-s.mu
	p := s.pending[threadID]
	s.mu <- struct{}{}
	return p
}

func (s *InterruptStore) Clear(threadID string) {
	<-s.mu
	delete(s.pending, threadID)
	s.mu <- struct{}{}
}

type Core struct {
	Runner         *runner.Runner
	SessionManager *SessionManager
	Interrupts     *InterruptStore
	AgentID        string
	OutputAgent    string // name of the agent whose output goes to choices[].delta.content
	Config         *config.Config
	Logger         zerolog.Logger
}

func NewCore(cfg *config.Config, agentCfg *config.AgentConfig, tools []tool.Tool, logger zerolog.Logger) (*Core, error) {
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

	sessionService := session.InMemoryService()
	sm := NewSessionManager(sessionService, cfg.AppName, logger)

	r, err := runner.New(runner.Config{
		AppName:        cfg.AppName,
		Agent:          agentInstance,
		SessionService: sessionService,
	})
	if err != nil {
		return nil, err
	}

	// Pre-create a default session for startup validation
	if err := sm.GetOrCreate(context.Background(), "default"); err != nil {
		logger.Warn().Err(err).Msg("failed to pre-create default session")
	}

	return &Core{
		Runner:         r,
		SessionManager: sm,
		Interrupts:     NewInterruptStore(),
		AgentID:        agentCfg.ID,
		Config:         cfg,
		Logger:         logger,
	}, nil
}

// NewCoreWithAgent creates a Core using a pre-built agent hierarchy (e.g. for multi-agent setups).
func NewCoreWithAgent(cfg *config.Config, agentCfg *config.AgentConfig, rootAgent adkagent.Agent, logger zerolog.Logger) (*Core, error) {
	sessionService := session.InMemoryService()
	sm := NewSessionManager(sessionService, cfg.AppName, logger)

	r, err := runner.New(runner.Config{
		AppName:        cfg.AppName,
		Agent:          rootAgent,
		SessionService: sessionService,
	})
	if err != nil {
		return nil, err
	}

	if err := sm.GetOrCreate(context.Background(), "default"); err != nil {
		logger.Warn().Err(err).Msg("failed to pre-create default session")
	}

	return &Core{
		Runner:         r,
		SessionManager: sm,
		Interrupts:     NewInterruptStore(),
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

