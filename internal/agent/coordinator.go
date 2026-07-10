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
	"google.golang.org/genai"
	"google.golang.org/adk/tool/toolconfirmation"
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

// RunHandle is the tracked state of one session's run.
type RunHandle struct {
	SessionID string    `json:"session_id"`
	UserID    string    `json:"user_id"`
	AgentID   string    `json:"agent_id"`
	Status    RunStatus `json:"status"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	cancel context.CancelFunc
}

// Coordinator owns background agent runs, decoupled from the HTTP connection.
// Each run executes in a detached goroutine that drives streamEvents into the
// EventLog; clients attach by reading the log. Runs survive client disconnect;
// a reconnecting client replays from ?after=<seq>.
type Coordinator struct {
	log    eventlog.EventLog
	logger zerolog.Logger
	now    func() time.Time

	mu     sync.Mutex
	active map[string]*RunHandle // currently running/queued, by sessionID
	known  map[string]*RunHandle // last-known state (incl. finished), by sessionID
	byUser map[string]map[string]bool
}

// NewCoordinator constructs a run coordinator over the given EventLog.
func NewCoordinator(log eventlog.EventLog, logger zerolog.Logger) *Coordinator {
	return &Coordinator{
		log:    log,
		logger: logger,
		now:    time.Now,
		active: map[string]*RunHandle{},
		known:  map[string]*RunHandle{},
		byUser: map[string]map[string]bool{},
	}
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
}

// Start launches (or attaches to) a background run for the session. If a run is
// already active for the session it returns the existing handle (the caller then
// attaches as a reader); otherwise it starts a new detached goroutine.
func (c *Coordinator) Start(req RunRequest) (*RunHandle, error) {
	c.mu.Lock()
	if h, ok := c.active[req.SessionID]; ok && (h.Status == RunRunning || h.Status == RunQueued) {
		c.mu.Unlock()
		return h, nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	h := &RunHandle{
		SessionID: req.SessionID,
		UserID:    req.UserID,
		AgentID:   req.Core.AgentID,
		Status:    RunRunning,
		StartedAt: c.now(),
		UpdatedAt: c.now(),
		cancel:    cancel,
	}
	c.active[req.SessionID] = h
	c.known[req.SessionID] = h
	if c.byUser[req.UserID] == nil {
		c.byUser[req.UserID] = map[string]bool{}
	}
	c.byUser[req.UserID][req.SessionID] = true
	c.mu.Unlock()

	runID := fmt.Sprintf("run-%s", uuid.New().String()[:12])
	_, _ = c.log.Append(runCtx, req.SessionID, eventlog.AgentEvent{V: 1, Type: eventlog.EvRunStatus, Status: eventlog.StatusRunning})

	go c.run(runCtx, req, h, runID)
	return h, nil
}

func (c *Coordinator) run(ctx context.Context, req RunRequest, h *RunHandle, runID string) {
	defer func() {
		c.mu.Lock()
		delete(c.active, req.SessionID)
		c.mu.Unlock()
		h.cancel()
	}()

	core := req.Core
	enc := newEventLogEncoder(ctx, c.log, req.SessionID)
	enc.Metadata(core.ModelID, core.AgentID, 0)
	enc.Progress("planning", "Analyzing...")

	if err := core.SessionManager.GetOrCreate(ctx, req.SessionID); err != nil {
		c.logger.Error().Err(err).Str("session", req.SessionID).Msg("coordinator: session create failed")
		enc.Progress("error", "Failed to create session")
		c.finish(ctx, req.SessionID, h, RunError, eventlog.StatusError, err.Error())
		return
	}

	if len(req.Messages) == 0 {
		c.finish(ctx, req.SessionID, h, RunError, eventlog.StatusError, "no messages")
		return
	}
	last := req.Messages[len(req.Messages)-1]
	content := genai.NewContentFromText(last.Content, genai.RoleUser)
	if req.Saver != nil {
		req.Saver.SaveUserMessage(ctx, req.SessionID, last.Content)
	}

	streamEvents(ctx, enc, core, req.SessionID, runID, content, req.Saver, c.logger)

	if enc.Interrupted() {
		c.finish(ctx, req.SessionID, h, RunAwaitingInput, eventlog.StatusAwaitingInput, "")
		return
	}
	c.finish(ctx, req.SessionID, h, RunDone, eventlog.StatusDone, "")
}

func (c *Coordinator) finish(ctx context.Context, sessionID string, h *RunHandle, status RunStatus, evStatus, errMsg string) {
	c.mu.Lock()
	h.Status = status
	h.UpdatedAt = c.now()
	c.mu.Unlock()
	_, _ = c.log.Append(ctx, sessionID, eventlog.AgentEvent{V: 1, Type: eventlog.EvRunStatus, Status: evStatus, Err: errMsg})
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
	h := &RunHandle{SessionID: sessionID, UserID: userID, AgentID: core.AgentID, Status: RunRunning, StartedAt: c.now(), UpdatedAt: c.now(), cancel: cancel}
	c.active[sessionID] = h
	c.known[sessionID] = h
	if c.byUser[userID] == nil {
		c.byUser[userID] = map[string]bool{}
	}
	c.byUser[userID][sessionID] = true
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.active, sessionID)
			c.mu.Unlock()
			cancel()
		}()
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

		runID := fmt.Sprintf("resume-%s", uuid.New().String()[:12])
		streamEvents(runCtx, enc, core, sessionID, runID, confirmContent, nil, c.logger)

		if enc.Interrupted() {
			c.finish(runCtx, sessionID, h, RunAwaitingInput, eventlog.StatusAwaitingInput, "")
			return
		}
		c.finish(runCtx, sessionID, h, RunDone, eventlog.StatusDone, "")
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

// Cancel stops an active run for the session.
func (c *Coordinator) Cancel(sessionID string) bool {
	c.mu.Lock()
	h, ok := c.active[sessionID]
	c.mu.Unlock()
	if !ok {
		return false
	}
	h.cancel()
	c.finish(context.Background(), sessionID, h, RunCancelled, eventlog.StatusCancelled, "")
	return true
}
