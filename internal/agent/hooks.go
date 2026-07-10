package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	internalagents "agentic/internal/agents"
	"agentic/internal/eventlog"
	"agentic/internal/types"
	"agentic/pkg/db/opensearch"
	pkgmemory "agentic/pkg/memory"

	"github.com/rs/zerolog"
	"google.golang.org/adk/session"
)

// ArchiveHook returns a PostRunHook that flushes the terminated session's event
// log to the OpenSearch cold archive + projected full-parts messages. No-op when
// the archiver's OpenSearch client is nil (degradation-safe).
func ArchiveHook(archiver *Archiver, app string) PostRunHook {
	return func(info PostRunInfo) {
		if archiver == nil {
			return
		}
		archiver.FlushAsync(app, info.UserID, info.SessionID)
	}
}

// MemoryExtractorHook returns a PostRunHook that runs the `memory_extractor`
// internal agent over the turn and writes the extracted durable facts to the
// per-user long-term memory store (OpenSearch, kNN). No-op when any dependency
// is nil. It only fires on a successful (done) run.
//
// NEEDS LIVE OpenSearch + a model provider to verify end-to-end: the extractor
// is a model call and the store is OpenSearch. Wiring/compile is exercised here;
// behaviour is not runtime-tested in this environment.
func MemoryExtractorHook(reg *internalagents.Registry, sessionSvc session.Service, mem *pkgmemory.Service, app string, logger zerolog.Logger) PostRunHook {
	log := logger.With().Str("hook", "memory_extractor").Logger()
	return func(info PostRunInfo) {
		if reg == nil || sessionSvc == nil || mem == nil || info.Status != RunDone {
			return
		}
		if reg.Get("memory_extractor") == nil {
			return
		}
		input := lastUserText(info.Messages)
		if strings.TrimSpace(input) == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := reg.Run(ctx, "memory_extractor", sessionSvc, info.UserID, input)
		if err != nil {
			log.Warn().Err(err).Str("session", info.SessionID).Msg("memory extraction failed")
			return
		}
		for _, fact := range splitFacts(out) {
			if _, err := mem.Add(ctx, app, info.UserID, fact); err != nil {
				log.Warn().Err(err).Msg("memory add failed")
			}
		}
	}
}

// TitleHook returns a PostRunHook that sets a thread's title (via the `title`
// internal agent) when it is still unset ("New Chat" / empty). No-op when any
// dependency is nil or the thread already has a user-set title. Fires only on a
// done run.
//
// NEEDS LIVE OpenSearch + a model provider to verify end-to-end.
func TitleHook(reg *internalagents.Registry, sessionSvc session.Service, os *opensearch.Client, logger zerolog.Logger) PostRunHook {
	log := logger.With().Str("hook", "title").Logger()
	return func(info PostRunInfo) {
		if reg == nil || sessionSvc == nil || os == nil || info.Status != RunDone {
			return
		}
		if reg.Get("title") == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if !titleUnset(ctx, os, info.SessionID) {
			return
		}
		input := lastUserText(info.Messages)
		if strings.TrimSpace(input) == "" {
			return
		}
		title, err := reg.Run(ctx, "title", sessionSvc, info.UserID, input)
		if err != nil {
			log.Warn().Err(err).Str("session", info.SessionID).Msg("title generation failed")
			return
		}
		title = cleanTitle(title)
		if title == "" {
			return
		}
		if err := os.UpdateDocument(ctx, opensearch.IndexThreads, info.SessionID, map[string]any{
			"title":      title,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			log.Warn().Err(err).Str("session", info.SessionID).Msg("title update failed")
		}
	}
}

// CompactionTriggerHook returns a PostRunHook that inspects the run's usage
// (the last EvUsage event in the session log) and, when the used context crosses
// the threshold (used >= context_window - reserved), signals that a compaction
// pass is due. It reads the durable usage signal from the log (per plan 01 §
// usage) rather than the live loop.
//
// The actual compaction (summarise the head, anchor the prior summary, reorder
// the projection sent to the model) requires the full conversation and a live
// model, so this hook currently DETECTS + LOGS the trigger and is the wiring
// point where CompactionService.CompactFull would be invoked. Enabling the
// summarise-and-reproject step is deferred (needs a live model to verify) and is
// intentionally not blocking this PR.
func CompactionTriggerHook(log eventlog.EventLog, reservedTokens int, logger zerolog.Logger) PostRunHook {
	hlog := logger.With().Str("hook", "compaction_trigger").Logger()
	if reservedTokens <= 0 {
		reservedTokens = 4000
	}
	return func(info PostRunInfo) {
		if log == nil || info.Status != RunDone {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		usage := lastUsage(ctx, log, info.SessionID)
		if usage == nil || usage.ContextWindow <= 0 {
			return
		}
		used := usage.ContextUsed
		if used == 0 {
			used = usage.PromptTokens
		}
		if used >= usage.ContextWindow-reservedTokens {
			hlog.Info().
				Str("session", info.SessionID).
				Int("used", used).
				Int("context_window", usage.ContextWindow).
				Int("reserved", reservedTokens).
				Msg("context window threshold crossed — compaction due (CompactionService.CompactFull is the plug-in point)")
		}
	}
}

// lastUsage drains the session log (non-follow) and returns the last usage
// payload, or nil if none.
func lastUsage(ctx context.Context, log eventlog.EventLog, sessionID string) *eventlog.UsagePayload {
	ch, err := log.Read(ctx, sessionID, 0, false)
	if err != nil {
		return nil
	}
	var last *eventlog.UsagePayload
	for se := range ch {
		if se.Event.Type == eventlog.EvUsage && se.Event.Usage != nil {
			last = se.Event.Usage
		}
	}
	return last
}

// titleUnset reports whether the thread's title is empty or the default. A GET
// failure (thread not found / OpenSearch down) reports false — don't overwrite
// what we can't read.
func titleUnset(ctx context.Context, os *opensearch.Client, threadID string) bool {
	hit, err := os.GetDocument(ctx, opensearch.IndexThreads, threadID)
	if err != nil {
		return false
	}
	var doc struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(hit.Source, &doc); err != nil {
		return false
	}
	t := strings.TrimSpace(doc.Title)
	return t == "" || t == "New Chat"
}

func lastUserText(msgs []types.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content
	}
	return ""
}

// splitFacts splits the extractor output into individual memory lines. The
// extractor may return a bullet/newline-separated list; blank lines and bullet
// markers are stripped.
func splitFacts(out string) []string {
	var facts []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*• ")
		line = strings.TrimSpace(line)
		if line != "" {
			facts = append(facts, line)
		}
	}
	return facts
}

// cleanTitle strips quotes/whitespace/trailing punctuation from a generated
// title and caps its length.
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	s = strings.TrimRight(s, ".!?")
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
