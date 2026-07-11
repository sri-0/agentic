package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentic/internal/eventlog"
	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

// Archiver flushes a session's hot event log to the durable OpenSearch cold
// archive (session_events) on terminal run status, and persists the projected
// full-parts assistant messages to the messages index for reload/rehydration.
//
// It also implements eventlog.ColdStore so a CompositeLog can replay from the
// archive once the hot Redis window (TTL) expires. All operations are no-ops
// when the OpenSearch client is nil (degradation-safe): the in-memory default
// path keeps working without OpenSearch.
type Archiver struct {
	os     *opensearch.Client
	log    eventlog.EventLog
	app    string
	logger zerolog.Logger
}

// NewArchiver constructs an Archiver over a hot EventLog and an OpenSearch
// client. A nil os client makes every method a safe no-op.
func NewArchiver(os *opensearch.Client, log eventlog.EventLog, app string, logger zerolog.Logger) *Archiver {
	return &Archiver{os: os, log: log, app: app, logger: logger.With().Str("component", "archiver").Logger()}
}

// FlushAsync reads the full session log and writes (a) the raw session_events
// archive and (b) the projected messages, in a detached goroutine so it never
// blocks the run goroutine's finish. Safe to call with a nil OpenSearch client.
func (a *Archiver) FlushAsync(app, userID, sessionID string) {
	if a == nil || a.os == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.Flush(ctx, app, userID, sessionID); err != nil {
			a.logger.Warn().Err(err).Str("session", sessionID).Msg("archive flush failed")
		}
	}()
}

// Flush synchronously reads the whole log for a session and writes the cold
// archive + projected messages. Exposed for tests / explicit flushes. The
// message docs are written without wait_for refresh (hot path); use
// FlushWaitRefresh on the terminal path when the write must be immediately
// searchable. Flush also satisfies the coordinator's TerminalFlusher; the
// coordinator calls FlushWaitRefresh directly so terminal writes refresh.
func (a *Archiver) Flush(ctx context.Context, app, userID, sessionID string) error {
	return a.flush(ctx, app, userID, sessionID, false)
}

// FlushWaitRefresh is Flush but blocks until the projected message docs are
// visible to search (refresh=wait_for). Used by the coordinator's synchronous
// terminal flush (Task C) so a reload right after `done` can search the fresh
// full-parts assistant message.
func (a *Archiver) FlushWaitRefresh(ctx context.Context, app, userID, sessionID string) error {
	return a.flush(ctx, app, userID, sessionID, true)
}

func (a *Archiver) flush(ctx context.Context, app, userID, sessionID string, waitRefresh bool) error {
	if a.os == nil {
		return nil
	}
	events, err := a.readAll(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("read session log: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	// (a) raw session_events — one doc per seq, gap-free, payload not indexed.
	for _, se := range events {
		payload, _ := json.Marshal(se.Event)
		var payloadObj map[string]any
		_ = json.Unmarshal(payload, &payloadObj)
		doc := map[string]any{
			"app_name":   app,
			"user_id":    userID,
			"session_id": sessionID,
			"seq":        se.Seq,
			"type":       string(se.Event.Type),
			"ts":         se.Event.Ts,
			"author":     se.Event.Author,
			"payload":    payloadObj,
		}
		docID := fmt.Sprintf("%s:%d", sessionID, se.Seq)
		if _, err := a.os.IndexDocument(ctx, opensearch.IndexSessionEvents, docID, doc); err != nil {
			a.logger.Warn().Err(err).Str("session", sessionID).Int64("seq", se.Seq).Msg("archive event index failed")
		}
	}

	// (b) projected full-parts assistant messages.
	agentEvents := make([]eventlog.AgentEvent, len(events))
	for i, se := range events {
		agentEvents[i] = se.Event
	}
	msgs := eventlog.ProjectMessages(agentEvents)
	for _, m := range msgs {
		parts, _ := json.Marshal(m.Parts)
		// Deterministic created_at derived from the turn's first event ts, so a
		// re-flush of an earlier turn keeps its original ordering timestamp (it must
		// stay < the next turn's user message). Falls back to now if unset.
		createdAt := time.Now().UTC().Format(time.RFC3339)
		if m.TsMillis > 0 {
			createdAt = time.UnixMilli(m.TsMillis).UTC().Format(time.RFC3339)
		}
		doc := map[string]any{
			"thread_id":  sessionID,
			"user_id":    userID,
			"role":       m.Role,
			"content":    m.Content,          // flattened for search / back-compat
			"parts":      json.RawMessage(parts), // full AI-SDK parts for rehydration
			"created_at": createdAt,
		}
		// Deterministic _id keyed by (session, turn, role): re-flushes of a growing
		// log UPSERT each assistant message in place (PUT _doc/{id}) instead of
		// appending a duplicate on every run terminal.
		docID := fmt.Sprintf("%s:%d:%s", sessionID, m.Turn, m.Role)
		if _, err := a.os.IndexDocumentRefresh(ctx, opensearch.IndexMessages, docID, doc, waitRefresh); err != nil {
			a.logger.Warn().Err(err).Str("session", sessionID).Msg("archive message index failed")
		}
	}
	a.logger.Info().Str("session", sessionID).Int("events", len(events)).Int("messages", len(msgs)).Msg("session archived")
	return nil
}

// ReadHistory implements eventlog.ColdStore: it reads the archived events for a
// session back from OpenSearch, ordered by seq, so a CompositeLog can replay the
// history after the hot Redis window expires.
func (a *Archiver) ReadHistory(ctx context.Context, sessionID string) ([]eventlog.SeqEvent, error) {
	if a.os == nil {
		return nil, nil
	}
	query := map[string]any{
		"size": 10000,
		"query": map[string]any{
			"term": map[string]any{"session_id": sessionID},
		},
		"sort": []any{map[string]any{"seq": map[string]any{"order": "asc"}}},
	}
	resp, err := a.os.Search(ctx, opensearch.IndexSessionEvents, query)
	if err != nil {
		return nil, err
	}
	out := make([]eventlog.SeqEvent, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var raw struct {
			Seq     int64           `json:"seq"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(hit.Source, &raw); err != nil {
			continue
		}
		var ev eventlog.AgentEvent
		if err := json.Unmarshal(raw.Payload, &ev); err != nil {
			continue
		}
		out = append(out, eventlog.SeqEvent{Seq: raw.Seq, Event: ev})
	}
	return out, nil
}

// readAll drains the full non-follow log for a session.
func (a *Archiver) readAll(ctx context.Context, sessionID string) ([]eventlog.SeqEvent, error) {
	ch, err := a.log.Read(ctx, sessionID, 0, false)
	if err != nil {
		return nil, err
	}
	var out []eventlog.SeqEvent
	for se := range ch {
		if se.Seq < 0 {
			continue // heartbeat
		}
		out = append(out, se)
	}
	return out, nil
}
