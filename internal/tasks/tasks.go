// Package tasks owns the live task-board representation and its mapping to the
// stream wire type. It is the single place that knows how a swarm/coordinator
// task board (and the todowrite tool's todo list) becomes a UI task list, so the
// stream loop and the agents stay decoupled from that detail.
package tasks

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"agentic/internal/stream"
)

// StateKeySuffix marks a session-state key carrying a JSON task board, e.g.
// "swarm:task_board" or "coordinator:task_board".
const StateKeySuffix = ":task_board"

// BoardTask is one entry of a swarm/coordinator task board (the worker-centric
// shape stored in session state).
type BoardTask struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Input  string `json:"input"`
	Status string `json:"status"` // pending | running | done | failed
	Result string `json:"result,omitempty"`
}

// statusToUI maps the worker-board status vocabulary to the UI task vocabulary.
func statusToUI(s string) string {
	switch s {
	case "running":
		return "in_progress"
	case "done":
		return "completed"
	case "failed":
		return "cancelled"
	default:
		return "pending"
	}
}

// title derives a short task title from a board task.
func title(t BoardTask) string {
	s := strings.TrimSpace(t.Input)
	if s == "" {
		s = t.Worker
	}
	if len(s) > 80 {
		s = strings.TrimSpace(s[:80]) + "…"
	}
	return s
}

// FromBoardJSON parses a JSON-encoded []BoardTask and maps it to stream tasks,
// stamping each task's owning worker as the agent. Returns nil on parse error.
func FromBoardJSON(raw string) []stream.Task {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var board []BoardTask
	if err := json.Unmarshal([]byte(raw), &board); err != nil {
		return nil
	}
	tasks := make([]stream.Task, 0, len(board))
	for i, t := range board {
		id := t.ID
		if id == "" {
			id = fmt.Sprintf("%d", i)
		}
		// Owner is the unique per-task worker instance (matches how dispatch
		// re-authors the worker's stream), so multiple tasks of the same worker
		// type are individually addressable in the UI.
		agent := t.Worker
		if agent != "" {
			agent = agent + "#" + id
		}
		tasks = append(tasks, stream.Task{
			ID:     id,
			Title:  title(t),
			Status: statusToUI(t.Status),
			Agent:  agent,
		})
	}
	return tasks
}

// BoardFromStateDelta extracts a task board (if present) from an adk event's
// state delta. delta is event.Actions.StateDelta. Returns the mapped tasks and
// whether a usable board was found. A board key whose value fails to parse (e.g.
// a coordinator that wrapped the JSON in markdown/reasoning) yields ok=false so
// the caller skips emitting a broken/empty board.
func BoardFromStateDelta(delta map[string]any) ([]stream.Task, bool) {
	for k, v := range delta {
		if !strings.HasSuffix(k, StateKeySuffix) {
			continue
		}
		if s, ok := v.(string); ok {
			board := FromBoardJSON(s)
			return board, board != nil
		}
	}
	return nil, false
}

// MapTodos converts a todowrite tool response ({todos:[{content,status,
// priority?}]}) into UI tasks. Status passes through (already UI vocabulary).
func MapTodos(response map[string]any) []stream.Task {
	rawTodos, _ := response["todos"].([]any)
	out := make([]stream.Task, 0, len(rawTodos))
	for i, rt := range rawTodos {
		todo, ok := rt.(map[string]any)
		if !ok {
			continue
		}
		str := func(key string) string {
			if v, ok := todo[key].(string); ok {
				return v
			}
			return ""
		}
		status := str("status")
		if status == "" {
			status = "pending"
		}
		out = append(out, stream.Task{
			ID:       fmt.Sprintf("%d", i),
			Title:    str("content"),
			Status:   status,
			Priority: str("priority"),
		})
	}
	return out
}

// statusRank orders UI task statuses so terminal states never regress.
func statusRank(s string) int {
	switch s {
	case "in_progress":
		return 1
	case "cancelled":
		return 2
	case "completed":
		return 3
	default: // pending
		return 0
	}
}

// Clamp makes the board monotonic across a run: a task never regresses below the
// highest status it has reached. `seen` is the per-run high-water map (mutated).
// This guards against a coordinator that re-emits a completed task as pending,
// so the UI board settles cleanly instead of flickering back.
func Clamp(seen map[string]string, board []stream.Task) {
	for i := range board {
		id := board[i].ID
		if prev := seen[id]; prev != "" && statusRank(prev) > statusRank(board[i].Status) {
			board[i].Status = prev
		} else {
			seen[id] = board[i].Status
		}
	}
}

// Signature is a stable digest of a task list, used to suppress duplicate
// emissions when an unchanged board is re-written each loop iteration.
func Signature(tasks []stream.Task) string {
	h := sha1.New()
	for _, t := range tasks {
		fmt.Fprintf(h, "%s\x1f%s\x1f%s\x1f%s\x1e", t.ID, t.Status, t.Agent, t.Title)
	}
	return hex.EncodeToString(h.Sum(nil))
}
