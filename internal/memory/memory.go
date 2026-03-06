// Package memory provides a future memory service interface.
package memory

import "context"

// Service defines the memory storage interface.
type Service interface {
	Store(ctx context.Context, key string, value any) error
	Retrieve(ctx context.Context, query string) ([]any, error)
}

// NoOpService is a no-op implementation of Service.
type NoOpService struct{}

func (NoOpService) Store(_ context.Context, _ string, _ any) error  { return nil }
func (NoOpService) Retrieve(_ context.Context, _ string) ([]any, error) { return nil, nil }
