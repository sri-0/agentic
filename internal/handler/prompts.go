package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"agentic/internal/types"
	"agentic/pkg/db/opensearch"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

// ── Handlers ────────────────────────────────────────────────────────────────

// PromptsCreate handles POST /v1/prompts.
func PromptsCreate(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p types.Prompt
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		if p.Version == 0 {
			p.Version = 1
		}
		p.CreatedAt = now
		p.UpdatedAt = now

		doc := map[string]any{
			"name":        p.Name,
			"description": p.Description,
			"template":    p.Template,
			"variables":   p.Variables,
			"tags":        p.Tags,
			"version":     p.Version,
			"created_at":  p.CreatedAt,
			"updated_at":  p.UpdatedAt,
		}

		id, err := os.IndexDocument(r.Context(), opensearch.IndexPrompts, p.ID, doc)
		if err != nil {
			logger.Error().Err(err).Msg("prompt create failed")
			http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
			return
		}

		p.ID = id
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(p)
	}
}

// PromptsGet handles GET /v1/prompts/{id}.
func PromptsGet(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		hit, err := os.GetDocument(r.Context(), opensearch.IndexPrompts, id)
		if err != nil {
			logger.Error().Err(err).Str("id", id).Msg("prompt get failed")
			http.Error(w, `{"error":"prompt not found"}`, http.StatusNotFound)
			return
		}

		var p types.Prompt
		if err := json.Unmarshal(hit.Source, &p); err != nil {
			http.Error(w, `{"error":"parse error"}`, http.StatusInternalServerError)
			return
		}
		p.ID = hit.ID

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	}
}

// PromptsUpdate handles PUT /v1/prompts/{id}.
func PromptsUpdate(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		var p types.Prompt
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

		doc := map[string]any{
			"updated_at": p.UpdatedAt,
		}
		if p.Name != "" {
			doc["name"] = p.Name
		}
		if p.Description != "" {
			doc["description"] = p.Description
		}
		if p.Template != "" {
			doc["template"] = p.Template
		}
		if p.Variables != nil {
			doc["variables"] = p.Variables
		}
		if p.Tags != nil {
			doc["tags"] = p.Tags
		}
		if p.Version > 0 {
			doc["version"] = p.Version
		}

		if err := os.UpdateDocument(r.Context(), opensearch.IndexPrompts, id, doc); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("prompt update failed")
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "updated"})
	}
}

// PromptsDelete handles DELETE /v1/prompts/{id}.
func PromptsDelete(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		if err := os.DeleteDocument(r.Context(), opensearch.IndexPrompts, id); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("prompt delete failed")
			http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// PromptsList handles GET /v1/prompts — list/search prompts.
func PromptsList(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		size := queryInt(r, "size", 20)
		tag := r.URL.Query().Get("tag")
		search := r.URL.Query().Get("q")

		query := map[string]any{
			"size": size,
			"sort": []map[string]any{{"updated_at": map[string]string{"order": "desc"}}},
		}

		if tag != "" {
			query["query"] = map[string]any{"term": map[string]any{"tags": tag}}
		} else if search != "" {
			query["query"] = map[string]any{
				"multi_match": map[string]any{
					"query":  search,
					"fields": []string{"name^2", "description"},
				},
			}
		} else {
			query["query"] = map[string]any{"match_all": map[string]any{}}
		}

		resp, err := os.Search(r.Context(), opensearch.IndexPrompts, query)
		if err != nil {
			logger.Error().Err(err).Msg("prompts list failed")
			http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
			return
		}

		prompts := make([]types.Prompt, 0, len(resp.Hits.Hits))
		for _, hit := range resp.Hits.Hits {
			var p types.Prompt
			if err := json.Unmarshal(hit.Source, &p); err != nil {
				continue
			}
			p.ID = hit.ID
			prompts = append(prompts, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total":   resp.Hits.Total.Value,
			"prompts": prompts,
		})
	}
}
