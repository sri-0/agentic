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

// evictIdleTTL is how long a finished session stays in the coordinator/log
// before the idle sweeper evicts it (M1). An active run is never evicted.
const evictIdleTTL = 30 * time.Minute

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
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	cancel   context.CancelFunc
	termOnce *sync.Once // guards the single terminal append + status set per run
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

	evicter interface {
		Evict(string)
		IdleSince(string, time.Time) bool
	}
	stopSweep chan struct{}
}

// NewCoordinator constructs a run coordinator over the given EventLog.
func NewCoordinator(log eventlog.EventLog, logger zerolog.Logger) *Coordinator {
	c := &Coordinator{
		log:       log,
		logger:    logger,
		now:       time.Now,
		active:    map[string]*RunHandle{},
		known:     map[string]*RunHandle{},
		byUser:    map[string]map[string]bool{},
		pending:   map[string][]RunRequest{},
		stopSweep: make(chan struct{}),
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

	// turnKey is a test-only label used by the run seam to distinguish turns; it
	// has no effect in production (defaultRunFunc ignores it).
	turnKey string
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
			StartSeq:  startSeq, // approximate; corrected when the turn actually runs
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
	h := &RunHandle{
		SessionID: req.SessionID,
		UserID:    req.UserID,
		AgentID:   agentIDOf(req),
		RunID:     runID,
		Status:    RunRunning,
		StartSeq:  startSeq,
		StartedAt: c.now(),
		UpdatedAt: c.now(),
		cancel:    cancel,
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
	return h
}

// headLocked returns Head(sessionID); caller holds mu. Errors map to 0.
func (c *Coordinator) headLocked(sessionID string) int64 {
	n, _ := c.log.Head(context.Background(), sessionID)
	return n
}

func (c *Coordinator) run(ctx context.Context, req RunRequest, h *RunHandle, runID string) {
	enc := newEventLogEncoder(ctx, c.log, req.SessionID)

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
		req.Saver.SaveUserMessage(ctx, req.SessionID, last.Content)
	}

	runID := fmt.Sprintf("run-%s", uuid.New().String()[:12])
	if err := streamEvents(ctx, enc, core, req.SessionID, runID, content, req.Saver, c.logger); err != nil {
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

		_, _ = c.log.Append(ctx, sessionID, eventlog.AgentEvent{
			V: 1, Type: eventlog.EvRunStatus, Status: statusToEvent(outcome.status), Err: outcome.err, RunID: runID,
		})
		if h.cancel != nil {
			h.cancel()
		}
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
// seamless continuation.
func (c *Coordinator) Resume(core *Core, sessionID, userID string, pending *hitl.PendingInterrupt, approved bool) (*RunHandle, error) {
	c.mu.Lock()
	if h, ok := c.active[sessionID]; ok && (h.Status == RunRunning || h.Status == RunQueued) {
		c.mu.Unlock()
		return h, fmt.Errorf("run already active for session %s", sessionID)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	runID := newRunID()
	startSeq := c.headLocked(sessionID) + 1
	h := &RunHandle{SessionID: sessionID, UserID: userID, AgentID: core.AgentID, RunID: runID, Status: RunRunning, StartSeq: startSeq, StartedAt: c.now(), UpdatedAt: c.now(), cancel: cancel}
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
		// Re-surface the tool call so a fresh reader sees it before the result.
		enc.put(eventlog.AgentEvent{V: 1, Type: eventlog.EvHITLResolved,
			Tool: &eventlog.ToolPayload{ID: pending.ToolCallID, Name: pending.ToolName, Args: pending.Details}})

		funcResponse := &genai.FunctionResponse{
			Name:     toolconfirmation.FunctionCallName,
			ID:       pending.ConfirmationCallID,
			Response: map[string]any{"confirmed": approved},
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

// Status returns the last-known handle for a session owned by userID.
func (c *Coordinator) Status(userID, sessionID string) (*RunHandle, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.known[sessionID]
	if !ok || (userID != "" && h.UserID != userID) {
		return nil, false
	}
	cp := *h
	return &cp, true
}

// List returns last-known handles for all sessions owned by userID.
func (c *Coordinator) List(userID string) []*RunHandle {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*RunHandle
	for sid := range c.byUser[userID] {
		if h, ok := c.known[sid]; ok {
			cp := *h
			out = append(out, &cp)
		}
	}
	return out
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
func (c *Coordinator) sweepLoop() {
	t := time.NewTicker(evictSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.sweep()
		case <-c.stopSweep:
			return
		}
	}
}

func (c *Coordinator) sweep() {
	cutoff := c.now().Add(-evictIdleTTL)
	c.mu.Lock()
	var evict []string
	for sid, h := range c.known {
		if _, active := c.active[sid]; active {
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
