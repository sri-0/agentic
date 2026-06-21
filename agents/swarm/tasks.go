package swarm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Task represents a unit of work on the swarm task board.
type Task struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Input  string `json:"input"`
	Status string `json:"status"` // "pending", "running", "done", "failed"
	Result string `json:"result,omitempty"`
}

// parseTaskBoard tolerantly extracts the []Task array from a coordinator's
// output. Models sometimes wrap it in markdown fences or in a state object like
// {"swarm:task_board": [...], "swarm:iteration": 1} instead of emitting a bare
// array — handle all of those.
func parseTaskBoard(raw string) ([]Task, error) {
	s := strings.TrimSpace(raw)

	// Strip ```json ... ``` fences.
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}

	// Bare array (the expected shape).
	var tasks []Task
	if err := json.Unmarshal([]byte(s), &tasks); err == nil {
		return tasks, nil
	}

	// Object wrapper: pull the array out of a *task_board / tasks key.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err == nil {
		for k, v := range obj {
			if strings.HasSuffix(k, "task_board") || k == "tasks" {
				if err := json.Unmarshal(v, &tasks); err == nil {
					return tasks, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("not a task board")
}
