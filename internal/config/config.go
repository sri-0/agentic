package config

import (
	"context"
	"fmt"

	pkgvalkey "agentic/pkg/db/valkey"

	"github.com/sethvargo/go-envconfig"
)

type Config struct {
	Port      int    `env:"PORT,default=8000"`
	Host      string `env:"HOST,default=0.0.0.0"`
	AppName   string `env:"APP_NAME,default=agentic"`
	ConfigDir string `env:"CONFIG_DIR,default=config/default"`
	LogLevel  string `env:"LOG_LEVEL,default=info"`
	LogJSON   bool   `env:"LOG_JSON,default=false"`

	OpenSearchURL      string `env:"OPENSEARCH_URL,default=http://localhost:9200"`
	OpenSearchUsername  string `env:"OPENSEARCH_USERNAME"`
	OpenSearchPassword  string `env:"OPENSEARCH_PASSWORD"`

	Valkey       *pkgvalkey.Config `env:",noinit"`
	HITLStore    string           `env:"HITL_STORE,default=memory"`    // "memory" or "valkey"
	SessionStore string           `env:"SESSION_STORE,default=memory"` // "memory" or "valkey"

	// Loaded from YAML files
	Models *ModelsConfig
	Agents *AgentsConfig
	RAG    *RAGConfig
}

func Load(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return &cfg, nil
}
