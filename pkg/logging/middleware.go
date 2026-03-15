package logging

import (
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

const (
	KeyTraceID    = "trace_id"
	KeySpanID     = "span_id"
	ValNotPresent = "null"
)

func CreateLoggingMiddleware(logger zerolog.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			ip := r.RemoteAddr
			method := r.Method
			path := r.URL.Path
			userAgent := r.UserAgent()

			// get status code
			crw := &customResponseWriter{ResponseWriter: w, status: http.StatusOK}

			reqLogger := logger.With().
				Str("ip", ip).
				Str("method", method).
				Str("path", path).
				Str("user_agent", userAgent).
				Logger()
			ctx := reqLogger.WithContext(r.Context())
			r = r.WithContext(ctx)

			next.ServeHTTP(crw, r)

			reqLogger.Info().
				Int("status", crw.status).
				Dur("duration", time.Since(start)).
				Msg("HTTP Request")
		})
	}
}

// Wrapper for http.ResponseWriter to capture status code
type customResponseWriter struct {
	http.ResponseWriter
	status int
}

func (crw *customResponseWriter) WriteHeader(status int) {
	crw.status = status
	crw.ResponseWriter.WriteHeader(status)
}
