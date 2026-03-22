package opensearch

import (
	"context"
	"encoding/json"
)

const (
	IndexEmbeddings = "embeddings"
	IndexPrompts    = "prompts"
	IndexThreads    = "threads"
	IndexMessages   = "messages"
	IndexSkills     = "skills"

	// Default vector dimension (intfloat/multilingual-e5-large).
	DefaultVectorDimension = 1024
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
				"dimension": 1024,
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

// ThreadsMapping is the index mapping for the threads (chats) store.
var ThreadsMapping = json.RawMessage(`{
	"mappings": {
		"properties": {
			"user_id":    { "type": "keyword" },
			"title":      { "type": "text", "fields": { "keyword": { "type": "keyword" } } },
			"model":      { "type": "keyword" },
			"pinned":     { "type": "boolean" },
			"pinned_at":  { "type": "date" },
			"public":     { "type": "boolean" },
			"project_id": { "type": "keyword" },
			"created_at": { "type": "date" },
			"updated_at": { "type": "date" }
		}
	}
}`)

// MessagesMapping is the index mapping for the messages store.
var MessagesMapping = json.RawMessage(`{
	"mappings": {
		"properties": {
			"thread_id":        { "type": "keyword" },
			"user_id":          { "type": "keyword" },
			"role":             { "type": "keyword" },
			"content":          { "type": "text" },
			"parts":            { "type": "text", "index": false },
			"model":            { "type": "keyword" },
			"message_group_id": { "type": "keyword" },
			"created_at":       { "type": "date" }
		}
	}
}`)

// SkillsMapping is the index mapping for the skills store.
var SkillsMapping = json.RawMessage(`{
	"mappings": {
		"properties": {
			"name":        { "type": "keyword" },
			"description": { "type": "text" },
			"content":     { "type": "text", "index": false },
			"tags":        { "type": "keyword" },
			"version":     { "type": "integer" },
			"created_at":  { "type": "date" },
			"updated_at":  { "type": "date" }
		}
	}
}`)

// EnsureIndices creates all indices if they don't exist.
func EnsureIndices(ctx context.Context, client *Client) error {
	if err := client.CreateIndex(ctx, IndexEmbeddings, EmbeddingsMapping); err != nil {
		return err
	}
	if err := client.CreateIndex(ctx, IndexPrompts, PromptsMapping); err != nil {
		return err
	}
	if err := client.CreateIndex(ctx, IndexThreads, ThreadsMapping); err != nil {
		return err
	}
	if err := client.CreateIndex(ctx, IndexMessages, MessagesMapping); err != nil {
		return err
	}
	return client.CreateIndex(ctx, IndexSkills, SkillsMapping)
}
