// Package logbridge routes Go's standard "log" package output through zerolog.
// Call Setup once after bootstrap to ensure ADK launcher logs are consistent
// with the rest of the application.
package logbridge

import (
	"log"
	"strings"

	"github.com/rs/zerolog"
)

// Setup redirects the standard log package to write through the given zerolog
// logger at Info level. It strips the default timestamp/prefix flags since
// zerolog adds its own.
func Setup(logger zerolog.Logger) {
	log.SetFlags(0)
	log.SetOutput(&writer{logger: logger.With().Str("component", "adk").Logger()})
}

// writer adapts zerolog to io.Writer by creating a proper Info-level event for
// each line written by the standard log package.
type writer struct {
	logger zerolog.Logger
}

func (w *writer) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		w.logger.Info().Msg(msg)
	}
	return len(p), nil
}
