package server

import (
	"net/http"
	"time"

	"agentic/internal/agent"
	"agentic/internal/config"
	"agentic/internal/handler"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

// NewRouter creates the HTTP router with all routes and middleware.
func NewRouter(registry *agent.Registry, cfg *config.Config, logger zerolog.Logger) *mux.Router {
	r := mux.NewRouter()

	r.Use(corsMiddleware)
	r.Use(loggingMiddleware(logger))

	r.HandleFunc("/health", handler.Health(registry)).Methods("GET")
	r.HandleFunc("/v1/models", handler.Models(cfg)).Methods("GET")
	r.HandleFunc("/v1/chat/completions", handler.Chat(registry, cfg, logger)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/embeddings", handler.Embeddings(cfg, logger)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/messages", handler.Messages(cfg, logger)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/agent/resume", handler.Resume(registry, logger)).Methods("POST", "OPTIONS")

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
