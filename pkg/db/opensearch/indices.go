package opensearch

import (
	"context"
	"encoding/json"
)

const (
	IndexEmbeddings = "embeddings"
	IndexPrompts    = "prompts"

	// Default vector dimension (OpenAI text-embedding-3-small).
	DefaultVectorDimension = 1536
)

// EmbeddingsMapping is the index mapping for the embeddings vector store.
var EmbeddingsMapping = json.RawMessage(`{
	"settings": {
		"index": {
			"knn": true,
			"knn.algo_param.ef_search": 256
		}
	},
	"mappings": {
		"properties": {
			"project":        { "type": "keyword" },
			"doc_id":         { "type": "keyword" },
			"chunk_id":       { "type": "integer" },
			"title":          { "type": "text", "fields": { "keyword": { "type": "keyword" } } },
			"source":         { "type": "keyword" },
			"author":         { "type": "keyword" },
			"date":           { "type": "date", "format": "yyyy-MM-dd||epoch_millis" },
			"classification": { "type": "keyword" },
			"text":           { "type": "text" },
			"vector": {
				"type":      "knn_vector",
				"dimension": 1536,
				"method": {
					"name":       "hnsw",
					"space_type": "cosinesimil",
					"engine":     "lucene"
				}
			}
		}
	}
}`)

// PromptsMapping is the index mapping for the prompts store.
var PromptsMapping = json.RawMessage(`{
	"mappings": {
		"properties": {
			"name":        { "type": "keyword" },
			"description": { "type": "text" },
			"template":    { "type": "text", "index": false },
			"variables":   { "type": "keyword" },
			"tags":        { "type": "keyword" },
			"version":     { "type": "integer" },
			"created_at":  { "type": "date" },
			"updated_at":  { "type": "date" }
		}
	}
}`)

// EnsureIndices creates both indices if they don't exist.
func EnsureIndices(ctx context.Context, client *Client) error {
	if err := client.CreateIndex(ctx, IndexEmbeddings, EmbeddingsMapping); err != nil {
		return err
	}
	return client.CreateIndex(ctx, IndexPrompts, PromptsMapping)
}
