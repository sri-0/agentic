// Package rag provides a future RAG pipeline interface.
package rag

import "context"

// Document represents a retrieved document.
type Document struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Source  string  `json:"source"`
	Score   float64 `json:"score"`
}

// Pipeline defines the RAG search interface.
type Pipeline interface {
	Search(ctx context.Context, query string, topK int) ([]Document, error)
}

// NoOpPipeline is a no-op implementation of Pipeline.
type NoOpPipeline struct{}

func (NoOpPipeline) Search(_ context.Context, _ string, _ int) ([]Document, error) {
	return nil, nil
}
