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

	h, err := core.Start(RunRequest{SessionID: sessionID, UserID: userID, Core: agentCore, Messages: messages, Saver: saver})
	if err != nil {
		logger.Error().Err(err).Str("session", sessionID).Msg("coordinator start failed")
		enc.RunStarted()
		enc.Progress("error", "Failed to start run")
		enc.RunFinished(stream.Usage{})
		return
	}
	// Run-attach: read from StartSeq-1 so this reader only sees THIS run's events
	// (fixes multi-turn — a 2nd turn attaches after turn 1's terminal and closes
	// only at turn 2's terminal). afterSeq is clamped >= 0 by the log.
	after := h.StartSeq - 1
	ch, err := core.Log().Read(ctx, sessionID, after, true)
	if err != nil {
		enc.RunStarted()
		enc.RunFinished(stream.Usage{})
		return
	}
	PumpEventLog(ctx, ch, enc, PumpRunAttach)
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
	// Session-follow: replay then stay live emitting finish framing per terminal
	// until the client disconnects (ctx). A follower may watch many runs/turns.
	PumpEventLog(ctx, ch, enc, PumpSessionFollow)
}
