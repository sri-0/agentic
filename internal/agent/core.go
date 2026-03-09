package agent

import (
	"agentic/internal/config"

	"github.com/rs/zerolog"
	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
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

// ToolCaller executes tools by name, outside the ADK framework.
type ToolCaller interface {
	Call(name string, args map[string]any, threadID, callID string) (map[string]any, error)
}

// Core holds the ADK agent, runner, and supporting services.
type Core struct {
	Runner         *runner.Runner
	SessionManager *SessionManager
	HITLStore      *HITLStore
	ToolCaller     ToolCaller
	Config         *config.Config
	Logger         zerolog.Logger
}

// NewCore creates a fully wired Core with ADK agent, runner, and session management.
func NewCore(cfg *config.Config, tools []tool.Tool, hitlStore *HITLStore, toolCaller ToolCaller, logger zerolog.Logger) (*Core, error) {
	// Create OpenAI-compatible model backend
	llmModel := genaiopenai.New(genaiopenai.Config{
		APIKey:    cfg.LLMAPIKey,
		BaseURL:   cfg.LLMBaseURL,
		ModelName: cfg.LLMModel,
	})

	// Create LLM agent with tools
	agentInstance, err := llmagent.New(llmagent.Config{
		Name:        cfg.AppName,
		Description: "A helpful AI assistant with tool-calling capabilities",
		Model:       llmModel,
		Instruction: systemInstruction,
		Tools:       tools,
	})
	if err != nil {
		return nil, err
	}

	// Create session service and runner
	sessionService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        cfg.AppName,
		Agent:          agentInstance,
		SessionService: sessionService,
	})
	if err != nil {
		return nil, err
	}

	sm := NewSessionManager(sessionService, cfg.AppName, logger)

	return &Core{
		Runner:         r,
		SessionManager: sm,
		HITLStore:      hitlStore,
		ToolCaller:     toolCaller,
		Config:         cfg,
		Logger:         logger,
	}, nil
}
