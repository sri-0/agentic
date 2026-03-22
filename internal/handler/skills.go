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

// SkillsCreate handles POST /v1/skills.
func SkillsCreate(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var s types.Skill
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		if s.Version == 0 {
			s.Version = 1
		}
		s.CreatedAt = now
		s.UpdatedAt = now

		doc := map[string]any{
			"name":        s.Name,
			"description": s.Description,
			"content":     s.Content,
			"tags":        s.Tags,
			"version":     s.Version,
			"created_at":  s.CreatedAt,
			"updated_at":  s.UpdatedAt,
		}

		id, err := os.IndexDocument(r.Context(), opensearch.IndexSkills, s.ID, doc)
		if err != nil {
			logger.Error().Err(err).Msg("skill create failed")
			http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
			return
		}

		s.ID = id
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(s)
	}
}

// SkillsGet handles GET /v1/skills/{id}.
func SkillsGet(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		hit, err := os.GetDocument(r.Context(), opensearch.IndexSkills, id)
		if err != nil {
			logger.Error().Err(err).Str("id", id).Msg("skill get failed")
			http.Error(w, `{"error":"skill not found"}`, http.StatusNotFound)
			return
		}

		var s types.Skill
		if err := json.Unmarshal(hit.Source, &s); err != nil {
			http.Error(w, `{"error":"parse error"}`, http.StatusInternalServerError)
			return
		}
		s.ID = hit.ID

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	}
}

// SkillsUpdate handles PUT /v1/skills/{id}.
func SkillsUpdate(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		var s types.Skill
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

		doc := map[string]any{
			"updated_at": s.UpdatedAt,
		}
		if s.Name != "" {
			doc["name"] = s.Name
		}
		if s.Description != "" {
			doc["description"] = s.Description
		}
		if s.Content != "" {
			doc["content"] = s.Content
		}
		if s.Tags != nil {
			doc["tags"] = s.Tags
		}
		if s.Version > 0 {
			doc["version"] = s.Version
		}

		if err := os.UpdateDocument(r.Context(), opensearch.IndexSkills, id, doc); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("skill update failed")
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "updated"})
	}
}

// SkillsDelete handles DELETE /v1/skills/{id}.
func SkillsDelete(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		if err := os.DeleteDocument(r.Context(), opensearch.IndexSkills, id); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("skill delete failed")
			http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// SkillsList handles GET /v1/skills — list/search skills.
func SkillsList(os *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
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

		resp, err := os.Search(r.Context(), opensearch.IndexSkills, query)
		if err != nil {
			logger.Error().Err(err).Msg("skills list failed")
			http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
			return
		}

		skills := make([]types.Skill, 0, len(resp.Hits.Hits))
		for _, hit := range resp.Hits.Hits {
			var s types.Skill
			if err := json.Unmarshal(hit.Source, &s); err != nil {
				continue
			}
			s.ID = hit.ID
			skills = append(skills, s)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total":  resp.Hits.Total.Value,
			"skills": skills,
		})
	}
}
