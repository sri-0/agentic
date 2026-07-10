package agent

import (
	"context"
	"fmt"
	"net/http"

	"agentic/internal/chat"
	"agentic/internal/stream"
	"agentic/internal/stream/aisdk"
	"agentic/internal/stream/openai"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// newEncoderFor builds a wire-format encoder without needing a *Core (used when
// attaching to a session whose run may already be finished).
func newEncoderFor(format stream.Format, sink stream.Sink, requestID, modelID, agentID, threadID string) stream.Encoder {
	if format == stream.FormatAISDK {
		return aisdk.New(sink, modelID, agentID)
	}
	return openai.New(sink, requestID, agentID, threadID)
}

// StreamAgentRunBackground starts (or attaches to) a background run for the
// session via the coordinator, then streams the session's event log to the
// client. The run continues even if this connection drops; a reconnecting
// client resumes via StreamSessionAttach with ?after=<seq>.
func StreamAgentRunBackground(ctx context.Context, w http.ResponseWriter, format stream.Format, core *Coordinator, agentCore *Core, sessionID, userID string, messages []types.ChatMessage, saver *chat.MessageSaver, logger zerolog.Logger) {
	setStreamHeaders(w, format)
	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	enc := newEncoder(format, stream.NewSSESink(w), requestID, agentCore, sessionID)

	if _, err := core.Start(RunRequest{SessionID: sessionID, UserID: userID, Core: agentCore, Messages: messages, Saver: saver}); err != nil {
		logger.Error().Err(err).Str("session", sessionID).Msg("coordinator start failed")
		enc.RunStarted()
		enc.Progress("error", "Failed to start run")
		enc.RunFinished(stream.Usage{})
		return
	}
	ch, err := core.Log().Read(ctx, sessionID, 0, true)
	if err != nil {
		enc.RunStarted()
		enc.RunFinished(stream.Usage{})
		return
	}
	PumpEventLog(ctx, ch, enc)
}

// StreamSessionAttach attaches to an existing session's event log from afterSeq
// and streams live, reproducing the wire output exactly. Used by the reconnect /
// resume endpoint (GET /v1/sessions/{id}/stream?after=<seq>).
func StreamSessionAttach(ctx context.Context, w http.ResponseWriter, format stream.Format, coord *Coordinator, sessionID, modelID, agentID string, afterSeq int64, logger zerolog.Logger) {
	setStreamHeaders(w, format)
	requestID := fmt.Sprintf("chatcmpl-attach-%s", uuid.New().String()[:12])
	enc := newEncoderFor(format, stream.NewSSESink(w), requestID, modelID, agentID, sessionID)

	ch, err := coord.Log().Read(ctx, sessionID, afterSeq, true)
	if err != nil {
		enc.RunStarted()
		enc.RunFinished(stream.Usage{})
		return
	}
	PumpEventLog(ctx, ch, enc)
}
