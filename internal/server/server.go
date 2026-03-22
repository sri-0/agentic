package server

import (
	"net/http"
	"time"

	"agentic/internal/agent"
	"agentic/internal/config"
	"agentic/internal/handler"
	"agentic/pkg/db/opensearch"
	"agentic/pkg/memory"
	oa "agentic/pkg/openapi"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

// NewRouter creates the HTTP router with all routes and middleware.
func NewRouter(registry *agent.Registry, cfg *config.Config, osClient *opensearch.Client, memorySvc *memory.Service, logger zerolog.Logger) *mux.Router {
	r := mux.NewRouter()

	r.Use(corsMiddleware)
	r.Use(loggingMiddleware(logger))

	r.HandleFunc("/health", handler.Health(registry)).Methods("GET")
	specCfg := oa.SpecConfig{
		Title:       cfg.AppName,
		Description: "OpenAI-compatible API with agent orchestration, RAG, threads, and prompt management.",
		Version:     "1.0.0",
	}
	r.HandleFunc("/v1/openapi.json", handler.OpenAPISpec(r, specCfg)).Methods("GET")
	r.HandleFunc("/docs", handler.APIDocs(cfg.AppName)).Methods("GET")
	r.HandleFunc("/v1/models", handler.Models(cfg)).Methods("GET")
	r.HandleFunc("/v1/chat/completions", handler.Chat(registry, cfg, osClient, logger)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/embeddings", handler.Embeddings(cfg, logger)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/messages", handler.Messages(cfg, logger)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/agent/resume", handler.Resume(registry, logger)).Methods("POST", "OPTIONS")

	// RAG search endpoint (standalone vector/text search)
	if osClient != nil {
		r.HandleFunc("/v1/rag/search", handler.RAGSearch(osClient, logger)).Methods("POST", "OPTIONS")

		// Prompts CRUD
		r.HandleFunc("/v1/prompts", handler.PromptsList(osClient, logger)).Methods("GET")
		r.HandleFunc("/v1/prompts", handler.PromptsCreate(osClient, logger)).Methods("POST", "OPTIONS")
		r.HandleFunc("/v1/prompts/{id}", handler.PromptsGet(osClient, logger)).Methods("GET")
		r.HandleFunc("/v1/prompts/{id}", handler.PromptsUpdate(osClient, logger)).Methods("PUT", "OPTIONS")
		r.HandleFunc("/v1/prompts/{id}", handler.PromptsDelete(osClient, logger)).Methods("DELETE")

		// Threads (chats) CRUD
		r.HandleFunc("/v1/threads", handler.ThreadsList(osClient, logger)).Methods("GET")
		r.HandleFunc("/v1/threads", handler.ThreadsCreate(osClient, logger)).Methods("POST", "OPTIONS")
		r.HandleFunc("/v1/threads/{id}", handler.ThreadsGet(osClient, logger)).Methods("GET")
		r.HandleFunc("/v1/threads/{id}", handler.ThreadsUpdate(osClient, logger)).Methods("PUT", "OPTIONS")
		r.HandleFunc("/v1/threads/{id}", handler.ThreadsDelete(osClient, logger)).Methods("DELETE")

		// Skills CRUD
		r.HandleFunc("/v1/skills", handler.SkillsList(osClient, logger)).Methods("GET")
		r.HandleFunc("/v1/skills", handler.SkillsCreate(osClient, logger)).Methods("POST", "OPTIONS")
		r.HandleFunc("/v1/skills/{id}", handler.SkillsGet(osClient, logger)).Methods("GET")
		r.HandleFunc("/v1/skills/{id}", handler.SkillsUpdate(osClient, logger)).Methods("PUT", "OPTIONS")
		r.HandleFunc("/v1/skills/{id}", handler.SkillsDelete(osClient, logger)).Methods("DELETE")

		// Memories
		r.HandleFunc("/v1/memories", handler.MemoriesList(memorySvc, logger)).Methods("GET")
		r.HandleFunc("/v1/memories", handler.MemoriesAdd(memorySvc, logger)).Methods("POST", "OPTIONS")
		r.HandleFunc("/v1/memories/search", handler.MemoriesSearch(memorySvc, logger)).Methods("POST", "OPTIONS")
		r.HandleFunc("/v1/memories/{id}", handler.MemoriesUpdate(memorySvc, logger)).Methods("PUT", "OPTIONS")
		r.HandleFunc("/v1/memories/{id}", handler.MemoriesDelete(memorySvc, logger)).Methods("DELETE")

		// Thread messages
		r.HandleFunc("/v1/threads/{id}/messages", handler.ThreadsMessagesList(osClient, logger)).Methods("GET")
		r.HandleFunc("/v1/threads/{id}/messages", handler.ThreadsMessagesCreate(osClient, logger)).Methods("POST", "OPTIONS")
		r.HandleFunc("/v1/threads/{id}/messages/bulk", handler.ThreadsMessagesBulkCreate(osClient, logger)).Methods("POST", "OPTIONS")
		r.HandleFunc("/v1/threads/{id}/messages", handler.ThreadsMessagesDelete(osClient, logger)).Methods("DELETE")
	}

	return r
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(logger zerolog.Logger) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			logger.Info().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", sw.status).
				Dur("duration", time.Since(start)).
				Msg("request")
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Ensure statusWriter implements http.Flusher for SSE streaming.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
