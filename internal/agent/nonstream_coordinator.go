package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agentic/internal/chat"
	"agentic/internal/eventlog"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// NonStreamAgentRunCoordinated executes a stream:false turn THROUGH the run
// coordinator (like the streaming path) so every turn is durable and event-
// sourced, then collects the final assistant text from the event log and returns
// the standard OpenAI ChatCompletion JSON (M6). The response shape is identical
// to the direct NonStreamAgentRun; the difference is that the run is now
// recorded in the log (resumable/archivable) instead of bypassing it.
//
// It attaches a run reader from StartSeq-1 (so it only sees THIS turn) and
// coalesces output text-deltas until the run's terminal run-status, mirroring the
// projector's text rule. The connection context governs only the reader; the run
// itself continues detached if the client disconnects.
func NonStreamAgentRunCoordinated(ctx context.Context, w http.ResponseWriter, coord *Coordinator, core *Core, sessionID, userID string, messages []types.ChatMessage, rawUserText string, saver *chat.MessageSaver, logger zerolog.Logger) {
	requestID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	runLog := logger.With().Str("thread_id", sessionID).Str("agent_id", core.AgentID).Logger()

	h, err := coord.Start(RunRequest{SessionID: sessionID, UserID: userID, Core: core, Messages: messages, RawUserText: rawUserText, Saver: saver})
	if err != nil {
		runLog.Error().Err(err).Msg("non-stream: coordinator start failed")
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
		return
	}

	// Bound the wait so a hung run can't hold the HTTP handler forever; the run
	// continues detached regardless (durable in the log).
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	ch, err := coord.Log().Read(readCtx, sessionID, h.StartSeq-1, true)
	if err != nil {
		http.Error(w, `{"error":"failed to attach run"}`, http.StatusInternalServerError)
		return
	}

	var b strings.Builder
	finishReason := "stop"
	for se := range ch {
		ev := se.Event
		switch ev.Type {
		case eventlog.EvTextDelta:
			if ev.IsOutput {
				b.WriteString(ev.Text)
			}
		case eventlog.EvRunStatus:
			if ev.IsTerminal() {
				switch ev.Status {
				case eventlog.StatusError:
					finishReason = "error"
				case eventlog.StatusAwaitingInput:
					finishReason = "tool_calls"
				case eventlog.StatusCancelled:
					finishReason = "stop"
				}
				goto done
			}
		}
	}
done:

	resp := map[string]any{
		"id":      requestID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   core.AgentID,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": b.String()},
				"finish_reason": finishReason,
			},
		},
		"thread_id": sessionID,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	runLog.Info().Int("output_chars", b.Len()).Str("finish", finishReason).Msg("non-stream: coordinated run complete")
}
