package agent

import (
	"context"

	"agentic/internal/config"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	"github.com/rs/zerolog"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
)

const systemInstruction = `You are a helpful AI assistant with access to various tools. Use tools when needed to answer questions accurately. Think step by step and use the most appropriate tool for each task.

Available capabilities:
- Query the database for business data (products, orders, users, metrics)
- Write to the database (requires human approval)
- Search documents in the knowledge base
- Search the web for current information
- Perform mathematical calculations

Always provide clear, well-structured responses based on the data you retrieve.`

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
	Config         *config.Config
	Logger         zerolog.Logger
}

func NewCore(cfg *config.Config, tools []tool.Tool, logger zerolog.Logger) (*Core, error) {
	llmModel := genaiopenai.New(genaiopenai.Config{
		APIKey:    cfg.LLMAPIKey,
		BaseURL:   cfg.LLMBaseURL,
		ModelName: cfg.LLMModel,
	})

	agentInstance, err := llmagent.New(llmagent.Config{
		Name:        cfg.AppName,
		Model:       llmModel,
		Instruction: systemInstruction,
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
		Config:         cfg,
		Logger:         logger,
	}, nil
}
