package logging

import (
	"context"
	"fmt"
	stdlog "log"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"
)

// Setup initializes and returns a configured zerolog.Logger
func Setup() zerolog.Logger {
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
	zerolog.TimeFieldFormat = time.RFC3339Nano

	output := zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
		FormatMessage: func(i interface{}) string {
			return fmt.Sprintf("| %s |", i)
		},
		FormatCaller: func(i interface{}) string {
			return fmt.Sprintf("| %s", filepath.Base(fmt.Sprintf("%s", i)))
		},
	}

	var gitRevision string
	buildInfo, ok := debug.ReadBuildInfo()
	if ok {
		for _, v := range buildInfo.Settings {
			if v.Key == "vcs.revision" {
				gitRevision = v.Value
				break
			}
		}
	}

	logger := zerolog.New(output).
		Level(zerolog.TraceLevel).
		With().
		Timestamp().
		Caller().
		Int("pid", os.Getpid()).
		Str("git_revision", gitRevision).
		Str("go_version", buildInfo.GoVersion).
		Logger()

	stdlog.SetFlags(0)
	stdlog.SetOutput(logger)

	return logger
}

// Retrieves the logger from the context
func Get(ctx context.Context) *zerolog.Logger {
	return zerolog.Ctx(ctx)
}
