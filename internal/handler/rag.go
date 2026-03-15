package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"agentic/internal/rag"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

// RAGSearch handles POST /v1/rag/search — semantic vector search against the embeddings index.
func RAGSearch(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req rag.SearchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		if req.TopK <= 0 {
			req.TopK = 5
		}

		var resp *opensearch.SearchResponse
		var err error

		if len(req.Vector) > 0 {
			resp, err = os.KNNSearch(r.Context(), opensearch.IndexEmbeddings, "vector", req.Vector, req.TopK, req.Filters)
		} else {
			// Fall back to text search on the "text" field
			query := map[string]any{
				"size": req.TopK,
				"query": map[string]any{
					"match": map[string]any{
						"text": req.Query,
					},
				},
			}

			if len(req.Filters) > 0 {
				var filterClauses []map[string]any
				for k, v := range req.Filters {
					filterClauses = append(filterClauses, map[string]any{
						"term": map[string]any{k: v},
					})
				}
				query["query"] = map[string]any{
					"bool": map[string]any{
						"must":   []any{map[string]any{"match": map[string]any{"text": req.Query}}},
						"filter": filterClauses,
					},
				}
			}

			resp, err = os.Search(r.Context(), opensearch.IndexEmbeddings, query)
		}

		if err != nil {
			logger.Error().Err(err).Str("query", req.Query).Msg("rag search failed")
			http.Error(w, `{"error":"search failed"}`, http.StatusInternalServerError)
			return
		}

		results := make([]rag.SearchResult, 0, len(resp.Hits.Hits))
		for _, hit := range resp.Hits.Hits {
			results = append(results, rag.SearchResult{
				ID:    hit.ID,
				Score: hit.Score,
				Doc:   hit.Source,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rag.SearchResponse{
			Query:   req.Query,
			Total:   resp.Hits.Total.Value,
			Results: results,
		})
	}
}

// helper to parse int query params
func queryInt(r *http.Request, key string, defaultVal int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}
