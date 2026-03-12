package opensearch

import "encoding/json"

// SearchResponse represents an OpenSearch search result.
type SearchResponse struct {
	Hits SearchHits `json:"hits"`
}

// SearchHits contains the hits array and total count.
type SearchHits struct {
	Total SearchTotal `json:"total"`
	Hits  []Hit       `json:"hits"`
}

// SearchTotal has the total count of matching documents.
type SearchTotal struct {
	Value int `json:"value"`
}

// Hit is a single search result.
type Hit struct {
	ID     string          `json:"_id"`
	Score  float64         `json:"_score"`
	Source json.RawMessage `json:"_source"`
}

// BulkDoc is a document to be indexed in a bulk operation.
type BulkDoc struct {
	ID   string
	Body any
}
