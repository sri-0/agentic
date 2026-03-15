package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type RAGConfig struct {
	EmbeddingModel string `yaml:"embedding_model"`
	TopK           int    `yaml:"top_k"`
	Index          string `yaml:"index"`
	Prompt         string `yaml:"prompt"`
}

func LoadRAG(path string) (*RAGConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading rag config: %w", err)
	}
	var cfg RAGConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing rag config: %w", err)
	}
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}
	if cfg.Index == "" {
		cfg.Index = "embeddings"
	}
	return &cfg, nil
}
