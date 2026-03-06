package config

import (
	"context"
	"fmt"

	"github.com/sethvargo/go-envconfig"
)

type Config struct {
	Port           int    `env:"PORT,default=8000"`
	Host           string `env:"HOST,default=0.0.0.0"`
	LLMBaseURL     string `env:"LLM_BASE_URL,default=https://openrouter.ai/api/v1"`
	LLMAPIKey      string `env:"LLM_API_KEY,required"`
	LLMModel       string `env:"LLM_MODEL,default=openai/gpt-4o-mini"`
	AgentModelName string `env:"AGENT_MODEL_NAME,default=agent"`
	AppName        string `env:"APP_NAME,default=agentic"`
	LogLevel       string `env:"LOG_LEVEL,default=info"`
	LogJSON        bool   `env:"LOG_JSON,default=false"`
}

func Load(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return &cfg, nil
}
