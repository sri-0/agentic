package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agentic/internal/types"
	"agentic/pkg/db/opensearch"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

// getUserID extracts the user ID from the request. Delegates to the shared
// identity seam (handler.UserID); kept as a thin alias for existing callers.
func getUserID(r *http.Request) string {
	return UserID(r)
}

// ThreadsList returns all threads for the authenticated user, ordered by pinned then updated_at.
func ThreadsList(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)

		size := 100
		query := map[string]any{
			"size": size,
			"query": map[string]any{
				"term": map[string]any{"user_id": userID},
			},
			"sort": []any{
				map[string]any{"pinned": map[string]any{"order": "desc"}},
				map[string]any{"pinned_at": map[string]any{"order": "desc", "missing": "_last"}},
				map[string]any{"updated_at": map[string]any{"order": "desc"}},
			},
		}

		resp, err := osClient.Search(r.Context(), opensearch.IndexThreads, query)
		if err != nil {
			// Degrade gracefully when the store is unreachable (local dev without
			// OpenSearch): return an empty thread list so the sidebar still loads.
			logger.Warn().Err(err).Msg("threads list failed — returning empty list")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]types.Thread{})
			return
		}

		threads := make([]types.Thread, 0, len(resp.Hits.Hits))
		for _, hit := range resp.Hits.Hits {
			t := threadFromHit(hit)
			threads = append(threads, t)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(threads)
	}
}

// ThreadsCreate creates a new thread.
func ThreadsCreate(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)

		var req struct {
			Title     string  `json:"title"`
			Model     string  `json:"model"`
			ProjectID *string `json:"projectId,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invalid request: %s"}`, err), http.StatusBadRequest)
			return
		}

		if req.Title == "" {
			req.Title = "New Chat"
		}

		now := time.Now().UTC().Format(time.RFC3339)
		id := uuid.New().String()

		doc := map[string]any{
			"user_id":    userID,
			"title":      req.Title,
			"model":      req.Model,
			"pinned":     false,
			"pinned_at":  nil,
			"public":     false,
			"project_id": req.ProjectID,
			"created_at": now,
			"updated_at": now,
		}

		if _, err := osClient.IndexDocument(r.Context(), opensearch.IndexThreads, id, doc); err != nil {
			logger.Error().Err(err).Msg("thread create failed")
			http.Error(w, `{"error":"failed to create thread"}`, http.StatusInternalServerError)
			return
		}

		// Force refresh so the new thread is immediately searchable
		osClient.Refresh(r.Context(), opensearch.IndexThreads)

		thread := types.Thread{
			ID:        id,
			UserID:    userID,
			Title:     strPtr(req.Title),
			Model:     strPtr(req.Model),
			Pinned:    false,
			PinnedAt:  nil,
			Public:    false,
			ProjectID: req.ProjectID,
			CreatedAt: now,
			UpdatedAt: now,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"chat": thread})
	}
}

// ThreadsGet returns a single thread by ID.
func ThreadsGet(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		hit, err := osClient.GetDocument(r.Context(), opensearch.IndexThreads, id)
		if err != nil {
			logger.Error().Err(err).Str("id", id).Msg("thread get failed")
			http.Error(w, `{"error":"thread not found"}`, http.StatusNotFound)
			return
		}

		thread := threadFromHit(*hit)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(thread)
	}
}

// ThreadsUpdate updates a thread (title, model, pinned, etc.).
func ThreadsUpdate(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invalid request: %s"}`, err), http.StatusBadRequest)
			return
		}

		// Build update doc from allowed fields
		doc := make(map[string]any)
		for _, key := range []string{"title", "model", "pinned", "pinned_at", "public", "project_id"} {
			if v, ok := req[key]; ok {
				doc[key] = v
			}
		}

		// Handle pin toggle: set pinned_at automatically
		if pinned, ok := req["pinned"]; ok {
			if p, _ := pinned.(bool); p {
				doc["pinned_at"] = time.Now().UTC().Format(time.RFC3339)
			} else {
				doc["pinned_at"] = nil
			}
		}

		doc["updated_at"] = time.Now().UTC().Format(time.RFC3339)

		if err := osClient.UpdateDocument(r.Context(), opensearch.IndexThreads, id, doc); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("thread update failed")
			http.Error(w, `{"error":"failed to update thread"}`, http.StatusInternalServerError)
			return
		}

		// Return updated fields
		doc["id"] = id
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	}
}

// ThreadsDelete deletes a thread and all its messages.
func ThreadsDelete(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		// Delete the thread
		if err := osClient.DeleteDocument(r.Context(), opensearch.IndexThreads, id); err != nil {
			logger.Error().Err(err).Str("id", id).Msg("thread delete failed")
			http.Error(w, `{"error":"failed to delete thread"}`, http.StatusInternalServerError)
			return
		}

		// Delete all messages for this thread
		delQuery := map[string]any{
			"query": map[string]any{
				"term": map[string]any{"thread_id": id},
			},
		}
		if err := osClient.DeleteByQuery(r.Context(), opensearch.IndexMessages, delQuery); err != nil {
			logger.Warn().Err(err).Str("id", id).Msg("failed to delete thread messages")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}
}

// ThreadsMessagesList returns all messages for a thread, ordered by created_at.
func ThreadsMessagesList(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		threadID := r.URL.Query().Get("chatId")
		if threadID == "" {
			threadID = mux.Vars(r)["id"]
		}
		if threadID == "" {
			http.Error(w, `{"error":"missing thread id"}`, http.StatusBadRequest)
			return
		}

		query := map[string]any{
			"size": 1000,
			"query": map[string]any{
				"term": map[string]any{"thread_id": threadID},
			},
			"sort": []any{
				map[string]any{"created_at": map[string]any{"order": "asc"}},
			},
		}

		resp, err := osClient.Search(r.Context(), opensearch.IndexMessages, query)
		if err != nil {
			// Degrade gracefully: when the store is unreachable (e.g. OpenSearch
			// down in local dev), return an empty history with 200 so the chat UI
			// still mounts instead of erroring on load.
			logger.Warn().Err(err).Str("thread_id", threadID).Msg("messages list failed — returning empty history")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]types.ThreadMessage{})
			return
		}

		messages := make([]types.ThreadMessage, 0, len(resp.Hits.Hits))
		for _, hit := range resp.Hits.Hits {
			m := messageFromHit(hit)
			messages = append(messages, m)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(messages)
	}
}

// ThreadsMessagesCreate adds a message to a thread.
func ThreadsMessagesCreate(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		threadID := mux.Vars(r)["id"]

		var req struct {
			Role           string `json:"role"`
			Content        string `json:"content"`
			Parts          any    `json:"parts,omitempty"`
			Model          string `json:"model,omitempty"`
			UserID         string `json:"user_id,omitempty"`
			MessageGroupID string `json:"message_group_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invalid request: %s"}`, err), http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		id := uuid.New().String()

		doc := map[string]any{
			"thread_id":        threadID,
			"user_id":          req.UserID,
			"role":             req.Role,
			"content":          req.Content,
			"parts":            req.Parts,
			"model":            req.Model,
			"message_group_id": req.MessageGroupID,
			"created_at":       now,
		}

		if _, err := osClient.IndexDocument(r.Context(), opensearch.IndexMessages, id, doc); err != nil {
			logger.Error().Err(err).Msg("message create failed")
			http.Error(w, `{"error":"failed to create message"}`, http.StatusInternalServerError)
			return
		}

		// Also bump the thread's updated_at
		osClient.UpdateDocument(r.Context(), opensearch.IndexThreads, threadID, map[string]any{
			"updated_at": now,
		})

		msg := types.ThreadMessage{
			ID:             id,
			ThreadID:       threadID,
			UserID:         req.UserID,
			Role:           req.Role,
			Content:        req.Content,
			Parts:          req.Parts,
			Model:          req.Model,
			MessageGroupID: req.MessageGroupID,
			CreatedAt:      now,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(msg)
	}
}

// ThreadsMessagesBulkCreate adds multiple messages to a thread at once.
func ThreadsMessagesBulkCreate(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		threadID := mux.Vars(r)["id"]

		var req struct {
			Messages []struct {
				Role           string `json:"role"`
				Content        string `json:"content"`
				Parts          any    `json:"parts,omitempty"`
				Model          string `json:"model,omitempty"`
				UserID         string `json:"user_id,omitempty"`
				MessageGroupID string `json:"message_group_id,omitempty"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invalid request: %s"}`, err), http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		created := make([]types.ThreadMessage, 0, len(req.Messages))

		for _, m := range req.Messages {
			id := uuid.New().String()
			doc := map[string]any{
				"thread_id":        threadID,
				"user_id":          m.UserID,
				"role":             m.Role,
				"content":          m.Content,
				"parts":            m.Parts,
				"model":            m.Model,
				"message_group_id": m.MessageGroupID,
				"created_at":       now,
			}

			if _, err := osClient.IndexDocument(r.Context(), opensearch.IndexMessages, id, doc); err != nil {
				logger.Error().Err(err).Msg("bulk message create failed")
				http.Error(w, `{"error":"failed to create messages"}`, http.StatusInternalServerError)
				return
			}

			created = append(created, types.ThreadMessage{
				ID:             id,
				ThreadID:       threadID,
				UserID:         m.UserID,
				Role:           m.Role,
				Content:        m.Content,
				Parts:          m.Parts,
				Model:          m.Model,
				MessageGroupID: m.MessageGroupID,
				CreatedAt:      now,
			})
		}

		// Bump thread updated_at
		osClient.UpdateDocument(r.Context(), opensearch.IndexThreads, threadID, map[string]any{
			"updated_at": now,
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(created)
	}
}

// ThreadsMessagesDelete deletes messages from a thread (optionally after a cutoff timestamp).
func ThreadsMessagesDelete(osClient *opensearch.Client, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		threadID := mux.Vars(r)["id"]

		var req struct {
			After string `json:"after,omitempty"` // ISO timestamp — delete messages created after this
		}
		json.NewDecoder(r.Body).Decode(&req)

		var query map[string]any
		if req.After != "" {
			query = map[string]any{
				"query": map[string]any{
					"bool": map[string]any{
						"must": []any{
							map[string]any{"term": map[string]any{"thread_id": threadID}},
							map[string]any{"range": map[string]any{"created_at": map[string]any{"gt": req.After}}},
						},
					},
				},
			}
		} else {
			query = map[string]any{
				"query": map[string]any{
					"term": map[string]any{"thread_id": threadID},
				},
			}
		}

		if err := osClient.DeleteByQuery(r.Context(), opensearch.IndexMessages, query); err != nil {
			logger.Error().Err(err).Str("thread_id", threadID).Msg("messages delete failed")
			http.Error(w, `{"error":"failed to delete messages"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}
}

func threadFromHit(hit opensearch.Hit) types.Thread {
	var raw map[string]any
	json.Unmarshal(hit.Source, &raw)

	t := types.Thread{
		ID:        hit.ID,
		UserID:    getStr(raw, "user_id"),
		Pinned:    getBool(raw, "pinned"),
		Public:    getBool(raw, "public"),
		CreatedAt: getStr(raw, "created_at"),
		UpdatedAt: getStr(raw, "updated_at"),
	}
	if v := getStr(raw, "title"); v != "" {
		t.Title = &v
	}
	if v := getStr(raw, "model"); v != "" {
		t.Model = &v
	}
	if v := getStr(raw, "pinned_at"); v != "" {
		t.PinnedAt = &v
	}
	if v := getStr(raw, "project_id"); v != "" {
		t.ProjectID = &v
	}
	return t
}

func messageFromHit(hit opensearch.Hit) types.ThreadMessage {
	var raw map[string]any
	json.Unmarshal(hit.Source, &raw)

	m := types.ThreadMessage{
		ID:             hit.ID,
		ThreadID:       getStr(raw, "thread_id"),
		UserID:         getStr(raw, "user_id"),
		Role:           getStr(raw, "role"),
		Content:        getStr(raw, "content"),
		Model:          getStr(raw, "model"),
		MessageGroupID: getStr(raw, "message_group_id"),
		CreatedAt:      getStr(raw, "created_at"),
	}
	if v, ok := raw["parts"]; ok && v != nil {
		m.Parts = v
	}
	return m
}

func getStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getBool(m map[string]any, key string) bool {
	if v, ok := m[key]; ok && v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func strPtr(s string) *string {
	return &s
}
