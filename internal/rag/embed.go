package rag

import (
	"context"
	"fmt"

	"agentic/internal/config"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
)

// EmbedQuery calls the upstream embeddings API to get a vector for the given text.
// Uses the provider configured for the RAG embedding model via the openai-go client.
func EmbedQuery(ctx context.Context, cfg *config.Config, query string) ([]float64, error) {
	if cfg.RAG == nil || cfg.RAG.EmbeddingModel == "" {
		return nil, fmt.Errorf("rag.embedding_model not configured")
	}
	modelID := cfg.RAG.EmbeddingModel

	if cfg.Models == nil {
		return nil, fmt.Errorf("no models config loaded")
	}

	provider := cfg.Models.FindProviderForModel(modelID)
	if provider == nil {
		return nil, fmt.Errorf("no provider found for embedding model %s", modelID)
	}

	opts := []option.RequestOption{
		option.WithBaseURL(provider.BaseURL),
		option.WithAPIKey(provider.APIKey()),
	}

	if httpClient, _ := provider.HTTPClient(); httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}

	client := openai.NewClient(opts...)

	resp, err := client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: modelID,
		Input: openai.EmbeddingNewParamsInputUnion{
			OfString: param.NewOpt(query),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}

	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}

	return resp.Data[0].Embedding, nil
}
