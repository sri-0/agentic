package types

import "encoding/json"

type EmbeddingRequest struct {
	Model string `json:"model" jsonschema:"required" jsonschema_description:"Embedding model ID"`
	Input string `json:"input" jsonschema:"required" jsonschema_description:"Text to embed"`
}

type EmbeddingResponse struct {
	Object string          `json:"object" jsonschema_description:"Always 'list'"`
	Data   json.RawMessage `json:"data" jsonschema_description:"Embedding vectors"`
	Model  string          `json:"model" jsonschema_description:"Model used"`
	Usage  json.RawMessage `json:"usage,omitempty" jsonschema_description:"Token usage"`
}
