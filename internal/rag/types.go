package rag

import "encoding/json"

// API request/response types for the RAG search endpoint.

type SearchRequest struct {
	Query   string            `json:"query" jsonschema_description:"Search query text"`
	TopK    int               `json:"top_k" jsonschema_description:"Number of results (default 5)"`
	Filters map[string]string `json:"filters,omitempty" jsonschema_description:"Key-value filters on document metadata"`
	Vector  []float64         `json:"vector,omitempty" jsonschema_description:"Pre-computed embedding vector for KNN search"`
}

type SearchResponse struct {
	Query   string         `json:"query" jsonschema_description:"Original query"`
	Total   int            `json:"total" jsonschema_description:"Total matching documents"`
	Results []SearchResult `json:"results" jsonschema_description:"Ranked results"`
}

type SearchResult struct {
	ID    string          `json:"id" jsonschema_description:"Document ID"`
	Score float64         `json:"score" jsonschema_description:"Relevance score"`
	Doc   json.RawMessage `json:"document" jsonschema_description:"Document source data"`
}
