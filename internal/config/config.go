package config

import (
	"context"
	"fmt"

	"github.com/sethvargo/go-envconfig"
)

type Config struct {
	Port      int    `env:"PORT,default=8000"`
	Host      string `env:"HOST,default=0.0.0.0"`
	AppName   string `env:"APP_NAME,default=agentic"`
	ConfigDir string `env:"CONFIG_DIR,default=config/default"`
	LogLevel  string `env:"LOG_LEVEL,default=info"`
	LogJSON   bool   `env:"LOG_JSON,default=false"`

	// Loaded from YAML files
	Models *ModelsConfig
	Agents *AgentsConfig
}

func Load(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return &cfg, nil
}
