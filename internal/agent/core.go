package agent

import (
	"agentic/internal/config"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	"github.com/rs/zerolog"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

const systemInstruction = `You are a helpful AI assistant with access to various tools. Use tools when needed to answer questions accurately. Think step by step and use the most appropriate tool for each task.

Available capabilities:
- Query the database for business data (products, orders, users, metrics)
- Write to the database (requires human approval)
- Search documents in the knowledge base
- Search the web for current information
- Perform mathematical calculations

Always provide clear, well-structured responses based on the data you retrieve.`

type ToolCaller interface {
	Call(name string, args map[string]any, threadID, callID string) (map[string]any, error)
}

type Core struct {
	Model              model.LLM
	ToolDecls          []*genai.Tool
	Conversations      *ConversationStore
	HITLStore          *HITLStore
	ToolCaller         ToolCaller
	Config             *config.Config
	SystemInstruction  string
	Logger             zerolog.Logger
}

func NewCore(cfg *config.Config, tools []tool.Tool, hitlStore *HITLStore, toolCaller ToolCaller, logger zerolog.Logger) (*Core, error) {
	llmModel := genaiopenai.New(genaiopenai.Config{
		APIKey:    cfg.LLMAPIKey,
		BaseURL:   cfg.LLMBaseURL,
		ModelName: cfg.LLMModel,
	})

	toolDecls := buildToolDeclarations(tools)

	return &Core{
		Model:             llmModel,
		ToolDecls:         toolDecls,
		Conversations:     NewConversationStore(),
		HITLStore:         hitlStore,
		ToolCaller:        toolCaller,
		Config:            cfg,
		SystemInstruction: systemInstruction,
		Logger:            logger,
	}, nil
}

func buildToolDeclarations(tools []tool.Tool) []*genai.Tool {
	var decls []*genai.FunctionDeclaration
	for _, t := range tools {
		type declarator interface {
			Declaration() *genai.FunctionDeclaration
		}
		if d, ok := t.(declarator); ok {
			if decl := d.Declaration(); decl != nil {
				decls = append(decls, decl)
			}
		}
	}
	if len(decls) == 0 {
		return nil
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}
