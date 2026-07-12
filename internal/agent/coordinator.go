package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agentic/internal/chat"
	"agentic/internal/eventlog"
	"agentic/internal/hitl"
	"agentic/internal/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

// RunStatus is the lifecycle state of a background run.
type RunStatus string

const (
	RunQueued        RunStatus = "queued"
	RunRunning       RunStatus = "running"
	RunAwaitingInput RunStatus = "awaiting-input"
	RunDone          RunStatus = "done"
	RunError         RunStatus = "error"
	RunCancelled     RunStatus = "cancelled"
)

// defaultSessionRetention is the fallback retention window for a finished/idle
// session before the idle sweeper evicts it (M1) when none is configured. An
// active run is never evicted. Overridden per-Coordinator via SessionRetention
// (config SESSION_RETENTION).
const defaultSessionRetention = time.Hour

// evictSweepInterval is how often the sweeper runs.
const evictSweepInterval = 5 * time.Minute

// RunHandle is the tracked state of one session's run.
type RunHandle struct {
	SessionID string    `json:"session_id"`
	UserID    string    `json:"user_id"`
	AgentID   string    `json:"agent_id"`
	RunID     string    `json:"run_id"`
	Status    RunStatus `json:"status"`
	StartSeq  int64     `json:"start_seq"` // first seq THIS run will write (Head()+1 at Start)
	// Turn is the 0-based assistant turn this run writes, derived from the SAME
	// projection the archiver uses (eventlog.NextTurn), so the live start-frame
	// messageId ({session}:{turn}:assistant) matches the archived doc id. For a
	// QUEUED handle it is approximate (corrected when the turn actually runs),
	// mirroring StartSeq.
	Turn      int       `json:"turn"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Viewed is the per-user "seen the result" flag surfaced to the UI (Task B).
	// It is NOT persisted on the handle; the coordinator fills it from the
	// ViewedStore when returning a copy (Status/List). A finished-but-unviewed
	// session (done && !viewed) renders the completed ring. Running/queued
	// sessions have no meaningful viewed state and report false.
	Viewed bool `json:"viewed"`

	cancel   context.CancelFunc
	termOnce *sync.Once // guards the single terminal append + status set per run

	// captured for post-run hooks (archive/memory/title); not serialised.
	core     *Core
	messages []types.ChatMessage
}

// runOutcome is the terminal result of a run function, mapped by the coordinator
// to a run-status event. Exactly one lifecycle terminal per run.
type runOutcome struct {
	status RunStatus // RunDone | RunError | RunAwaitingInput
	err    string
}

// runFunc executes one turn, writing events through enc. It returns the terminal
// outcome. It is a seam: production uses defaultRunFunc (streamEvents); tests
// inject a stub without needing a model.
type runFunc func(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome

// Coordinator owns background agent runs, decoupled from the HTTP connection.
// Each run executes in a detached goroutine that drives the run function into
// the EventLog; clients attach by reading the log. Runs survive client
// disconnect; a reconnecting client replays from ?after=<seq>.
//
// Terminal state belongs to runs (RunID + StartSeq on the handle), not to the
// session log — a session log carries multiple turns. Concurrent turns for one
// session QUEUE (never dropped): the running goroutine drains the next queued
// turn on finish.
type Coordinator struct {
	log    eventlog.EventLog
	logger zerolog.Logger
	now    func() time.Time
	runFn  runFunc

	mu      sync.Mutex
	active  map[string]*RunHandle      // currently running/queued, by sessionID
	known   map[string]*RunHandle      // last-known state (incl. finished), by sessionID
	byUser  map[string]map[string]bool // sessionID set per user
	pending map[string][]RunRequest    // per-session FIFO of queued turns (C2)

	// sessionRetention governs how long a finished/idle session stays resumable
	// before the sweeper evicts it (Task A, config SESSION_RETENTION). Read at
	// sweep time; not a const so it can be configured per deployment.
	sessionRetention time.Duration

	evicter interface {
		Evict(string)
		IdleSince(string, time.Time) bool
	}
	stopSweep chan struct{}

	hooks       []PostRunHook           // fired async on every run terminal
	startHooks  []PostRunHook           // fired async when a fresh turn starts (status running)
	taskBoards  eventlog.TaskBoardStore // per-session current-board cache (Task 4)
	viewed      ViewedStore             // per-session per-user viewed flag (Task B)
	termFlusher TerminalFlusher         // synchronous terminal archive flush (Task C)
	app         string                  // app name for the terminal flush doc (Task C)
}

// TerminalFlusher persists the terminal turn's full-parts messages synchronously
// (via the archiver's Flush) so a reload immediately after `done` sees tool parts
// (Task C). It is awaited inside terminate with a bounded timeout before the
// (still-async) post-run hooks fire; the deterministic _id makes it idempotent.
type TerminalFlusher interface {
	Flush(ctx context.Context, app, userID, sessionID string) error
}

// TerminalFlusherFunc adapts a function to the TerminalFlusher interface, so a
// specific archiver method (e.g. FlushWaitRefresh) can be wired without the
// archiver having to name its terminal method Flush.
type TerminalFlusherFunc func(ctx context.Context, app, userID, sessionID string) error

// Flush implements TerminalFlusher.
func (f TerminalFlusherFunc) Flush(ctx context.Context, app, userID, sessionID string) error {
	return f(ctx, app, userID, sessionID)
}

// SetTerminalFlusher wires the synchronous terminal archive flush (Task C). app
// is the app name stamped on the flushed messages docs. Call during bootstrap
// wiring. When nil, terminate falls back to the async ArchiveHook alone (legacy
// behaviour).
func (c *Coordinator) SetTerminalFlusher(f TerminalFlusher, app string) {
	c.termFlusher = f
	c.app = app
}

// SetViewedStore wires the per-session viewed store (Task B). Call during
// bootstrap wiring. When nil, viewed is always reported false (running/queued
// semantics) and MarkViewed is a no-op.
func (c *Coordinator) SetViewedStore(s ViewedStore) { c.viewed = s }

// SetSessionRetention overrides the finished/idle session retention window used
// by the sweeper (Task A, config SESSION_RETENTION). Call during bootstrap
// wiring; a non-positive value keeps the default.
func (c *Coordinator) SetSessionRetention(d time.Duration) {
	if d > 0 {
		c.mu.Lock()
		c.sessionRetention = d
		c.mu.Unlock()
	}
}

// SetTaskBoardStore attaches a per-session task-board cache (Redis, or in-memory
// fallback). Call during bootstrap wiring; the encoder writes the current board
// on each task-list snapshot so a reconnect can render it without a full replay.
func (c *Coordinator) SetTaskBoardStore(s eventlog.TaskBoardStore) { c.taskBoards = s }

// TaskBoard returns the current task board snapshot for a session (empty if none
// or no store wired).
func (c *Coordinator) TaskBoard(ctx context.Context, sessionID string) []eventlog.TaskItem {
	if c.taskBoards == nil {
		return nil
	}
	items, _ := c.taskBoards.Get(ctx, sessionID)
	return items
}

// PostRunInfo is the context passed to a PostRunHook when a run reaches a
// terminal status. It carries enough to fire async side-effects (archive flush,
// memory extraction, auto-title, compaction) without holding the coordinator.
type PostRunInfo struct {
	SessionID string
	UserID    string
	AgentID   string
	RunID     string
	Status    RunStatus
	Core      *Core
	Messages  []types.ChatMessage // the turn's input messages (may be empty on resume)
}

// PostRunHook is a side-effect fired (non-blocking, async) exactly once per run
// terminal. Hooks must not block; the coordinator invokes each in its own
// goroutine. Registered via AddPostRunHook, they replace hardcoded post-run
// wiring so archive/memory/title concerns stay out of the run lifecycle.
type PostRunHook func(PostRunInfo)

// AddPostRunHook registers a post-run hook. Not safe to call concurrently with
// active runs; call during bootstrap wiring.
func (c *Coordinator) AddPostRunHook(h PostRunHook) {
	if h == nil {
		return
	}
	c.hooks = append(c.hooks, h)
}

// AddRunStartHook registers a hook fired async when a FRESH turn starts
// (Status is RunRunning, Messages carry the turn's input). Used for
// side-effects that should not wait for the terminal — e.g. generating the
// thread title from the first user message while the run is still streaming.
// Not safe to call concurrently with active runs; call during bootstrap wiring.
func (c *Coordinator) AddRunStartHook(h PostRunHook) {
	if h == nil {
		return
	}
	c.startHooks = append(c.startHooks, h)
}

// firePostRun invokes every registered hook in its own goroutine (non-blocking).
// Only fired on a hard terminal (done/error/cancelled), never on awaiting-input
// (the run is suspended, not finished).
func (c *Coordinator) firePostRun(info PostRunInfo) {
	if info.Status == RunAwaitingInput {
		return
	}
	c.fireHooks(c.hooks, info)
}

// fireHooks invokes each hook in its own panic-guarded goroutine (non-blocking).
func (c *Coordinator) fireHooks(hooks []PostRunHook, info PostRunInfo) {
	for _, h := range hooks {
		h := h
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error().Interface("panic", r).Str("session", info.SessionID).Msg("run hook panicked")
				}
			}()
			h(info)
		}()
	}
}

// NewCoordinator constructs a run coordinator over the given EventLog.
func NewCoordinator(log eventlog.EventLog, logger zerolog.Logger) *Coordinator {
	c := &Coordinator{
		log:              log,
		logger:           logger,
		now:              time.Now,
		active:           map[string]*RunHandle{},
		known:            map[string]*RunHandle{},
		byUser:           map[string]map[string]bool{},
		pending:          map[string][]RunRequest{},
		sessionRetention: defaultSessionRetention,
		stopSweep:        make(chan struct{}),
	}
	c.runFn = c.defaultRunFunc
	if ev, ok := log.(interface {
		Evict(string)
		IdleSince(string, time.Time) bool
	}); ok {
		c.evicter = ev
	}
	go c.sweepLoop()
	return c
}

// Log exposes the underlying EventLog so handlers can attach readers.
func (c *Coordinator) Log() eventlog.EventLog { return c.log }

// RunRequest describes a new turn to execute.
type RunRequest struct {
	SessionID string
	UserID    string
	Core      *Core
	Messages  []types.ChatMessage
	Saver     *chat.MessageSaver

	// RawUserText, when non-empty, is the ORIGINAL user text to PERSIST for this
	// turn (via SaveUserMessage). The model still receives the last message's
	// (possibly augmented, e.g. memory-recall-prepended) content, but the reload
	// history shows exactly what the user typed. Empty → persist the last message
	// content verbatim (legacy behaviour).
	RawUserText string

	// turnKey is a test-only label used by the run seam to distinguish turns; it
	// has no effect in production (defaultRunFunc ignores it).
	turnKey string

	// turn is stamped by startLocked (from eventlog.NextTurn) before the run
	// goroutine launches, so defaultRunFunc can persist the turn's user message
	// under the deterministic {session}:{turn}:user doc id.
	turn int
}

// Start launches a background run for the session. If a run is already active
// for the session the new turn is ENQUEUED (never dropped, C2) and a queued
// handle is returned; the caller still attaches to the log and will see the
// queued turn's events when the current run finishes and drains it. Otherwise a
// new detached goroutine starts immediately.
//
// The returned handle carries StartSeq (the first seq this turn will write) so a
// run-attach reader can attach from StartSeq-1 and only see this turn's events.
func (c *Coordinator) Start(req RunRequest) (*RunHandle, error) {
	c.mu.Lock()
	// H2: a session last-known to belong to another user cannot be started/
	// continued by this user.
	if h, ok := c.known[req.SessionID]; ok && req.UserID != "" && h.UserID != "" && h.UserID != req.UserID {
		c.mu.Unlock()
		return nil, fmt.Errorf("session %s is owned by another user", req.SessionID)
	}
	if h, ok := c.active[req.SessionID]; ok && (h.Status == RunRunning || h.Status == RunQueued) {
		// A run is active — queue this turn, do not drop it.
		c.pending[req.SessionID] = append(c.pending[req.SessionID], req)
		startSeq := c.headLocked(req.SessionID) + int64(len(c.pending[req.SessionID]))
		qh := &RunHandle{
			SessionID: req.SessionID,
			UserID:    req.UserID,
			AgentID:   agentIDOf(req),
			RunID:     newRunID(),
			Status:    RunQueued,
			StartSeq:  startSeq,                               // approximate; corrected when the turn actually runs
			Turn:      h.Turn + len(c.pending[req.SessionID]), // approximate; corrected when the turn actually runs
			StartedAt: c.now(),
			UpdatedAt: c.now(),
		}
		c.mu.Unlock()
		return qh, nil
	}

	h := c.startLocked(req)
	c.mu.Unlock()
	return h, nil
}

// startLocked creates the handle, stamps StartSeq/RunID, records tracking maps,
// appends run-status{running}, and launches the run goroutine. Caller holds mu.
func (c *Coordinator) startLocked(req RunRequest) *RunHandle {
	runCtx, cancel := context.WithCancel(context.Background())
	runID := newRunID()
	startSeq := c.headLocked(req.SessionID) + 1
	turn := c.nextTurnLocked(req.SessionID)
	req.turn = turn
	h := &RunHandle{
		SessionID: req.SessionID,
		UserID:    req.UserID,
		AgentID:   agentIDOf(req),
		RunID:     runID,
		Status:    RunRunning,
		StartSeq:  startSeq,
		Turn:      turn,
		StartedAt: c.now(),
		UpdatedAt: c.now(),
		cancel:    cancel,
		core:      req.Core,
		messages:  req.Messages,
	}
	h.termOnce = &sync.Once{}
	c.active[req.SessionID] = h
	c.known[req.SessionID] = h
	if c.byUser[req.UserID] == nil {
		c.byUser[req.UserID] = map[string]bool{}
	}
	c.byUser[req.UserID][req.SessionID] = true

	_, _ = c.log.Append(runCtx, req.SessionID, eventlog.AgentEvent{V: 1, Type: eventlog.EvRunStatus, Status: eventlog.StatusRunning, RunID: runID})

	go c.run(runCtx, req, h, runID)

	// Fire run-START hooks (e.g. early thread titling) async, with the turn's
	// input messages — they must never block or observe the run's outcome.
	c.fireHooks(c.startHooks, PostRunInfo{
		SessionID: req.SessionID,
		UserID:    req.UserID,
		AgentID:   agentIDOf(req),
		RunID:     runID,
		Status:    RunRunning,
		Core:      req.Core,
		Messages:  req.Messages,
	})
	return h
}

// headLocked returns Head(sessionID); caller holds mu. Errors map to 0.
func (c *Coordinator) headLocked(sessionID string) int64 {
	n, _ := c.log.Head(context.Background(), sessionID)
	return n
}

// nextTurnLocked derives the 0-based turn index the next run's assistant
// message will receive by folding the session's full log through the SAME
// projection the archiver uses for its deterministic {session}:{turn}:{role}
// doc ids (eventlog.NextTurn). The live start-frame messageId and the archived
// DB id therefore stay aligned by construction. Caller holds mu; read errors
// map to turn 0 (fresh session).
func (c *Coordinator) nextTurnLocked(sessionID string) int {
	ch, err := c.log.Read(context.Background(), sessionID, 0, false)
	if err != nil {
		return 0
	}
	var events []eventlog.AgentEvent
	for se := range ch {
		if se.Seq < 0 {
			continue // heartbeat
		}
		events = append(events, se.Event)
	}
	return eventlog.NextTurn(events)
}

func (c *Coordinator) run(ctx context.Context, req RunRequest, h *RunHandle, runID string) {
	enc := newEventLogEncoder(ctx, c.log, req.SessionID)
	enc.taskBoards = c.taskBoards

	outcome := c.runFn(ctx, req, enc)
	// HITL interrupt takes precedence over the run function's nominal outcome.
	if enc.Interrupted() {
		outcome = runOutcome{status: RunAwaitingInput}
	}

	c.finish(ctx, req, h, runID, outcome)
}

// defaultRunFunc is the production run: drive streamEvents into the event log
// and map its error to an outcome (H5: failed runs report error, not done).
func (c *Coordinator) defaultRunFunc(ctx context.Context, req RunRequest, enc *eventLogEncoder) runOutcome {
	core := req.Core
	enc.Metadata(core.ModelID, core.AgentID, 0)
	enc.Progress("planning", "Analyzing...")

	if err := core.SessionManager.GetOrCreate(ctx, req.SessionID); err != nil {
		c.logger.Error().Err(err).Str("session", req.SessionID).Msg("coordinator: session create failed")
		enc.Progress("error", "Failed to create session")
		return runOutcome{status: RunError, err: err.Error()}
	}

	if len(req.Messages) == 0 {
		return runOutcome{status: RunError, err: "no messages"}
	}
	last := req.Messages[len(req.Messages)-1]
	content := genai.NewContentFromText(last.Content, genai.RoleUser)
	if req.Saver != nil {
		// Persist the RAW user text (what the user typed), not the augmented
		// content sent to the model (e.g. memory-recall preamble). The model still
		// receives last.Content above; only the reload history is the raw text.
		// req.turn keys the doc deterministically ({session}:{turn}:user) so the
		// persisted user message pairs with the turn's assistant message id.
		saveText := last.Content
		if req.RawUserText != "" {
			saveText = req.RawUserText
		}
		req.Saver.SaveUserMessage(ctx, req.SessionID, req.UserID, saveText, req.turn)
	}

	runID := fmt.Sprintf("run-%s", uuid.New().String()[:12])
	// The assistant message is persisted by the archive post-run hook with FULL
	// PARTS (projected from the event log), so streamEvents is given a nil saver
	// here to avoid a duplicate text-only assistant row. The user message is
	// already saved above. Assistant-message persistence requires OpenSearch, and
	// the archive hook (which also requires OpenSearch) owns it — so there is no
	// persistence gap when the store is absent (both are no-ops).
	if err := streamEvents(ctx, enc, core, req.SessionID, runID, content, nil, c.logger); err != nil {
		return runOutcome{status: RunError, err: err.Error()}
	}
	return runOutcome{status: RunDone}
}

// finish is the run goroutine's natural completion. It terminates the run
// (idempotent per run via termOnce — if Cancel already terminated it, this is a
// no-op so no second terminal is appended, H5), then drains the next queued turn
// for the session (C2).
func (c *Coordinator) finish(ctx context.Context, req RunRequest, h *RunHandle, runID string, outcome runOutcome) {
	c.terminate(ctx, req.SessionID, h, runID, outcome)

	c.mu.Lock()
	next, hasNext := c.dequeueLocked(req.SessionID)
	c.mu.Unlock()
	if hasNext {
		c.mu.Lock()
		c.startLocked(next)
		c.mu.Unlock()
	}
}

// terminate stamps the terminal status + appends the terminal run-status event
// exactly once per run (guarded by h.termOnce). The FIRST caller wins: whether
// that is the run's natural finish or Cancel, the run has exactly one terminal.
func (c *Coordinator) terminate(ctx context.Context, sessionID string, h *RunHandle, runID string, outcome runOutcome) {
	h.termOnce.Do(func() {
		c.mu.Lock()
		h.Status = outcome.status
		h.UpdatedAt = c.now()
		delete(c.active, sessionID)
		c.mu.Unlock()

		// Task C: persist the terminal turn's FULL-PARTS messages SYNCHRONOUSLY
		// (bounded) BEFORE appending the terminal run-status event. The terminal
		// event is what a streaming/non-stream reader waits on to consider the run
		// `done`, so flushing first guarantees the full-parts assistant doc (with
		// tool parts) is already written+searchable by the time any client can
		// observe completion and reload history — closing the "tools don't render
		// after stream" race. ProjectMessages flushes the open assistant message at
		// end-of-stream, so it does not need the terminal event in the log. The
		// deterministic _id ({session}:{turn}:{role}) keeps this idempotent with the
		// later async ArchiveHook re-flush (which also captures the terminal event in
		// the raw session_events archive). Skipped on awaiting-input (turn not
		// complete) and when no flusher is wired (legacy async path). Non-terminal
		// paths are unaffected.
		if c.termFlusher != nil && outcome.status != RunAwaitingInput {
			fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := c.termFlusher.Flush(fctx, c.app, h.UserID, sessionID); err != nil {
				c.logger.Warn().Err(err).Str("session", sessionID).Msg("terminal archive flush failed; async hook will retry")
			}
			fcancel()
		}

		// Task B: on a HARD terminal the owner has not yet seen the result — record
		// the session UNVIEWED so the UI can render the completed-but-unseen ring.
		// awaiting-input is a suspension, not a terminal, so it is skipped (viewed
		// stays irrelevant). TTL aligns to session retention so the flag self-cleans
		// when the session drops out of /v1/sessions.
		if c.viewed != nil && outcome.status != RunAwaitingInput {
			vctx, vcancel := context.WithTimeout(context.Background(), 2*time.Second)
			ttl := c.sessionRetention
			if ttl <= 0 {
				ttl = defaultSessionRetention
			}
			if err := c.viewed.SetUnviewed(vctx, sessionID, ttl); err != nil {
				c.logger.Warn().Err(err).Str("session", sessionID).Msg("set unviewed failed")
			}
			vcancel()
		}

		// Terminal run-status event: appended AFTER the synchronous flush above so a
		// reader can never observe `done` before the parts doc exists (Task C).
		_, _ = c.log.Append(ctx, sessionID, eventlog.AgentEvent{
			V: 1, Type: eventlog.EvRunStatus, Status: statusToEvent(outcome.status), Err: outcome.err, RunID: runID,
		})
		if h.cancel != nil {
			h.cancel()
		}

		// Fire post-run hooks (archive flush, memory extraction, auto-title,
		// compaction) async, exactly once per run terminal. awaiting-input is not
		// a terminal for this purpose (firePostRun filters it). The archive flush
		// here is idempotent with the synchronous flush above (same deterministic
		// _id) and still covers the case where no terminal flusher is wired.
		c.firePostRun(PostRunInfo{
			SessionID: sessionID,
			UserID:    h.UserID,
			AgentID:   h.AgentID,
			RunID:     runID,
			Status:    outcome.status,
			Core:      h.core,
			Messages:  h.messages,
		})
	})
}

// dequeueLocked pops the next pending turn for the session. Caller holds mu.
func (c *Coordinator) dequeueLocked(sessionID string) (RunRequest, bool) {
	q := c.pending[sessionID]
	if len(q) == 0 {
		return RunRequest{}, false
	}
	next := q[0]
	q = q[1:]
	if len(q) == 0 {
		delete(c.pending, sessionID)
	} else {
		c.pending[sessionID] = q
	}
	return next, true
}

// Resume continues a suspended (awaiting-input) run after a HITL/question reply,
// appending to the same session log so attached and reconnecting clients see a
// seamless continuation. answers (one selected-label list per question) and text
// (optional free-text) ride the ADK confirmation payload so the question tool can
// surface them to the model; they are empty for a plain HITL approve/deny.
//
// Ownership is enforced: a session last-known to belong to a different user is
// rejected (H2). The returned handle's StartSeq lets the caller run-attach from
// StartSeq-1 so the continuation both streams and is recorded (C4).
func (c *Coordinator) Resume(core *Core, sessionID, userID string, pending *hitl.PendingInterrupt, approved bool, answers [][]string, text string) (*RunHandle, error) {
	c.mu.Lock()
	if h, ok := c.known[sessionID]; ok && userID != "" && h.UserID != "" && h.UserID != userID {
		c.mu.Unlock()
		return nil, fmt.Errorf("session %s is owned by another user", sessionID)
	}
	if h, ok := c.active[sessionID]; ok && (h.Status == RunRunning || h.Status == RunQueued) {
		c.mu.Unlock()
		return h, fmt.Errorf("run already active for session %s", sessionID)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	runID := newRunID()
	startSeq := c.headLocked(sessionID) + 1
	// NextTurn over a log suspended on awaiting-input reports the still-OPEN
	// assistant message's turn, so the continuation streams under the SAME
	// message id the interrupted run started with.
	turn := c.nextTurnLocked(sessionID)
	h := &RunHandle{SessionID: sessionID, UserID: userID, AgentID: core.AgentID, RunID: runID, Status: RunRunning, StartSeq: startSeq, Turn: turn, StartedAt: c.now(), UpdatedAt: c.now(), cancel: cancel, core: core}
	h.termOnce = &sync.Once{} // required: finish()/Cancel route through terminate() which calls termOnce.Do
	c.active[sessionID] = h
	c.known[sessionID] = h
	if c.byUser[userID] == nil {
		c.byUser[userID] = map[string]bool{}
	}
	c.byUser[userID][sessionID] = true
	_, _ = c.log.Append(runCtx, sessionID, eventlog.AgentEvent{V: 1, Type: eventlog.EvRunStatus, Status: eventlog.StatusRunning, RunID: runID})
	c.mu.Unlock()

	go func() {
		enc := newEventLogEncoder(runCtx, c.log, sessionID)
		enc.taskBoards = c.taskBoards
		// Re-surface the tool call so a fresh reader sees it before the result.
		// Kind records the decision (approved/denied) so the history projection
		// marks the folded data-tool-interrupt part resolved with the right badge.
		resolvedKind := eventlog.KindApproved
		if !approved {
			resolvedKind = eventlog.KindDenied
		}
		enc.put(eventlog.AgentEvent{V: 1, Type: eventlog.EvHITLResolved, Kind: resolvedKind,
			Tool: &eventlog.ToolPayload{ID: pending.ToolCallID, Name: pending.ToolName, Args: pending.Details}})

		// Build the confirmation FunctionResponse. ADK marshals Response to JSON
		// then unmarshals it into toolconfirmation.ToolConfirmation{Confirmed,
		// Payload} (see request_confirmation_processor.go), so "confirmed" →
		// .Confirmed and "payload" → .Payload. The question tool reads .Payload
		// via ctx.ToolConfirmation() and returns the answers to the model (C3).
		funcResponse := &genai.FunctionResponse{
			Name:     toolconfirmation.FunctionCallName,
			ID:       pending.ConfirmationCallID,
			Response: buildConfirmationResponse(approved, answers, text),
		}
		confirmContent := &genai.Content{Role: string(genai.RoleUser), Parts: []*genai.Part{{FunctionResponse: funcResponse}}}

		streamRunID := fmt.Sprintf("resume-%s", uuid.New().String()[:12])
		err := streamEvents(runCtx, enc, core, sessionID, streamRunID, confirmContent, nil, c.logger)

		outcome := runOutcome{status: RunDone}
		if err != nil {
			outcome = runOutcome{status: RunError, err: err.Error()}
		}
		if enc.Interrupted() {
			outcome = runOutcome{status: RunAwaitingInput}
		}
		c.finish(runCtx, RunRequest{SessionID: sessionID, UserID: userID}, h, runID, outcome)
	}()
	return h, nil
}

// Status returns the last-known handle for a session owned by userID. The
// returned copy carries the per-user Viewed flag (Task B) filled from the
// ViewedStore.
func (c *Coordinator) Status(userID, sessionID string) (*RunHandle, bool) {
	c.mu.Lock()
	h, ok := c.known[sessionID]
	if !ok || (userID != "" && h.UserID != userID) {
		c.mu.Unlock()
		return nil, false
	}
	cp := *h
	c.mu.Unlock()
	cp.Viewed = c.viewedFlag(&cp)
	return &cp, true
}

// List returns last-known handles for all sessions owned by userID. Each copy
// carries the per-user Viewed flag (Task B).
func (c *Coordinator) List(userID string) []*RunHandle {
	c.mu.Lock()
	var out []*RunHandle
	for sid := range c.byUser[userID] {
		if h, ok := c.known[sid]; ok {
			cp := *h
			out = append(out, &cp)
		}
	}
	c.mu.Unlock()
	for _, h := range out {
		h.Viewed = c.viewedFlag(h)
	}
	return out
}

// viewedFlag resolves the per-session viewed flag for a handle copy. Only a
// hard-terminal (done/error/cancelled) session has meaningful viewed state; a
// running/queued/awaiting-input session reports false (viewed is irrelevant). A
// nil store or lookup error reports false (fail-safe: the UI just won't show the
// unseen ring rather than falsely claiming viewed).
func (c *Coordinator) viewedFlag(h *RunHandle) bool {
	if c.viewed == nil {
		return false
	}
	switch h.Status {
	case RunDone, RunError, RunCancelled:
	default:
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := c.viewed.Viewed(ctx, h.SessionID)
	if err != nil {
		return false
	}
	return v
}

// MarkViewed marks a session as viewed by its owner (Task B). Ownership is
// enforced: returns false if the session is unknown or owned by another user
// (the handler maps that to 404). A nil viewed store makes this a no-op that
// still reports ownership success.
func (c *Coordinator) MarkViewed(userID, sessionID string) bool {
	c.mu.Lock()
	h, ok := c.known[sessionID]
	owned := ok && (userID == "" || h.UserID == "" || h.UserID == userID)
	c.mu.Unlock()
	if !ok || !owned {
		return false
	}
	if c.viewed != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := c.viewed.MarkViewed(ctx, sessionID); err != nil {
			c.logger.Warn().Err(err).Str("session", sessionID).Msg("mark viewed failed")
		}
	}
	return true
}

// Cancel stops an active run for the session. It terminates the run through the
// same once-guarded path as a natural finish, so Cancel's terminal WINS (it
// fires termOnce first) and the run goroutine's later finish() is a no-op — no
// double terminal, no status flip back to done (H5). Any queued turns are
// dropped since the user cancelled the session.
func (c *Coordinator) Cancel(sessionID string) bool {
	c.mu.Lock()
	h, ok := c.active[sessionID]
	if !ok {
		c.mu.Unlock()
		return false
	}
	delete(c.pending, sessionID)
	c.mu.Unlock()

	c.terminate(context.Background(), sessionID, h, h.RunID, runOutcome{status: RunCancelled})
	return true
}

// Confirmation payload keys. These are the JSON keys inside the ADK
// FunctionResponse Response map. "confirmed" and "payload" are consumed by ADK
// (→ ToolConfirmation.Confirmed / .Payload); the question tool reads the answers
// out of .Payload by the same key names, so they are shared here to keep the
// encode (Resume) and decode (questionHandler) sides in lockstep.
const (
	confirmKeyConfirmed = "confirmed"
	confirmKeyPayload   = "payload"
	confirmKeyAnswers   = "answers"
	confirmKeyText      = "text"
)

// buildConfirmationResponse produces the ADK FunctionResponse Response map. The
// answers/text are nested under "payload" so they land on
// ToolConfirmation.Payload after ADK's JSON round-trip. When there are no
// answers (plain HITL approve/deny) the payload is omitted entirely.
func buildConfirmationResponse(approved bool, answers [][]string, text string) map[string]any {
	resp := map[string]any{confirmKeyConfirmed: approved}
	if len(answers) > 0 || text != "" {
		payload := map[string]any{}
		if len(answers) > 0 {
			payload[confirmKeyAnswers] = answers
		}
		if text != "" {
			payload[confirmKeyText] = text
		}
		resp[confirmKeyPayload] = payload
	}
	return resp
}

func newRunID() string { return fmt.Sprintf("run-%s", uuid.New().String()[:12]) }

func agentIDOf(req RunRequest) string {
	if req.Core != nil {
		return req.Core.AgentID
	}
	return ""
}

func statusToEvent(s RunStatus) string {
	switch s {
	case RunError:
		return eventlog.StatusError
	case RunAwaitingInput:
		return eventlog.StatusAwaitingInput
	case RunCancelled:
		return eventlog.StatusCancelled
	default:
		return eventlog.StatusDone
	}
}

// sweepLoop periodically evicts idle finished sessions from the coordinator maps
// and the event log (M1). It never evicts a session with an active run.
//
// The tick period is min(evictSweepInterval, sessionRetention): the default 5-min
// cadence is preserved for the default 1h retention, but a SHORT configured
// retention (e.g. SESSION_RETENTION=10s) is swept just as promptly so a finished
// session actually drops from /v1/sessions within ~retention rather than waiting
// up to 5 minutes. Re-computed each tick so a retention set after construction
// (SetSessionRetention during bootstrap) takes effect.
func (c *Coordinator) sweepLoop() {
	t := time.NewTimer(c.sweepInterval())
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.sweep()
			t.Reset(c.sweepInterval())
		case <-c.stopSweep:
			return
		}
	}
}

// sweepInterval returns the sweeper tick period: the 5-min default, clamped down
// to the retention window so short retentions evict promptly.
func (c *Coordinator) sweepInterval() time.Duration {
	c.mu.Lock()
	retention := c.sessionRetention
	c.mu.Unlock()
	if retention <= 0 {
		retention = defaultSessionRetention
	}
	if retention < evictSweepInterval {
		return retention
	}
	return evictSweepInterval
}

func (c *Coordinator) sweep() {
	c.mu.Lock()
	retention := c.sessionRetention
	if retention <= 0 {
		retention = defaultSessionRetention
	}
	cutoff := c.now().Add(-retention)
	var evict []string
	for sid, h := range c.known {
		if _, active := c.active[sid]; active {
			continue
		}
		// A paused HITL run (awaiting-input) is suspended, not finished — it must
		// stay resumable regardless of age, so never evict it.
		if h.Status == RunAwaitingInput {
			continue
		}
		if h.UpdatedAt.After(cutoff) {
			continue
		}
		if c.evicter != nil && !c.evicter.IdleSince(sid, cutoff) {
			continue
		}
		evict = append(evict, sid)
	}
	for _, sid := range evict {
		h := c.known[sid]
		delete(c.known, sid)
		delete(c.pending, sid)
		if h != nil {
			if u := c.byUser[h.UserID]; u != nil {
				delete(u, sid)
				if len(u) == 0 {
					delete(c.byUser, h.UserID)
				}
			}
		}
	}
	c.mu.Unlock()

	if c.evicter != nil {
		for _, sid := range evict {
			c.evicter.Evict(sid)
		}
	}
}

// StopSweeper stops the background idle sweeper (for tests / shutdown).
func (c *Coordinator) StopSweeper() {
	select {
	case <-c.stopSweep:
	default:
		close(c.stopSweep)
	}
}
