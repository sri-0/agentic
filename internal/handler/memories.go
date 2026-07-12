package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"agentic/pkg/memory"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

// MemoriesSearch handles POST /v1/memories/search.
func MemoriesSearch(svc *memory.Service, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AppName string `json:"app_name"`
			UserID  string `json:"user_id"`
			Query   string `json:"query"`
			Count   int    `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if req.AppName == "" {
			req.AppName = "agentic"
		}
		if req.UserID == "" {
			req.UserID = "anonymous"
		}

		entries, err := svc.Search(r.Context(), req.AppName, req.UserID, req.Query, req.Count)
		if err != nil {
			logger.Error().Err(err).Msg("memory search failed")
			http.Error(w, `{"error":"search failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total":    len(entries),
			"memories": entries,
		})
	}
}

// MemoriesAdd handles POST /v1/memories.
func MemoriesAdd(svc *memory.Service, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AppName string `json:"app_name"`
			UserID  string `json:"user_id"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			http.Error(w, `{"error":"content is required"}`, http.StatusBadRequest)
			return
		}
		if req.AppName == "" {
			req.AppName = "agentic"
		}
		if req.UserID == "" {
			req.UserID = "anonymous"
		}

		id, err := svc.Add(r.Context(), req.AppName, req.UserID, req.Content)
		switch {
		case errors.Is(err, memory.ErrDuplicateMemory):
			// Near-duplicate of an existing memory: no new document created.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "duplicate"})
			return
		case errors.Is(err, memory.ErrJunkMemory):
			// Empty/negative/unknown fact — not persisted.
			http.Error(w, `{"error":"content is empty or a negative/unknown fact"}`, http.StatusUnprocessableEntity)
			return
		case err != nil:
			logger.Error().Err(err).Msg("memory add failed")
			http.Error(w, `{"error":"add failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"})
	}
}

// MemoriesList handles GET /v1/memories?app_name=&user_id=&count=.
func MemoriesList(svc *memory.Service, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appName := r.URL.Query().Get("app_name")
		if appName == "" {
			appName = "agentic"
		}
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			userID = "anonymous"
		}
		count := queryInt(r, "count", 50)

		entries, err := svc.List(r.Context(), appName, userID, count)
		if err != nil {
			logger.Error().Err(err).Msg("memory list failed")
			http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total":    len(entries),
			"memories": entries,
		})
	}
}

// MemoriesUpdate handles PUT /v1/memories/{id}.
func MemoriesUpdate(svc *memory.Service, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		var req struct {
			AppName string `json:"app_name"`
			UserID  string `json:"user_id"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			http.Error(w, `{"error":"content is required"}`, http.StatusBadRequest)
			return
		}
		if req.AppName == "" {
			req.AppName = "agentic"
		}
		if req.UserID == "" {
			req.UserID = "anonymous"
		}

		if err := svc.Update(r.Context(), req.AppName, req.UserID, id, req.Content); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("memory update failed")
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "updated"})
	}
}

// MemoriesDelete handles DELETE /v1/memories/{id}?app_name=&user_id=.
func MemoriesDelete(svc *memory.Service, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]
		appName := r.URL.Query().Get("app_name")
		if appName == "" {
			appName = "agentic"
		}
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			userID = "anonymous"
		}

		if err := svc.Delete(r.Context(), appName, userID, id); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("memory delete failed")
			http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
