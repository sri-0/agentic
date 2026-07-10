package coordinator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Task represents a unit of work on the coordinator task board.
type Task struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Input  string `json:"input"`
	Status string `json:"status"` // "pending", "running", "done", "failed"
	Result string `json:"result,omitempty"`
}

// parseTaskBoard tolerantly extracts the []Task array from the coordinator's
// output: bare array, markdown-fenced, or wrapped in a state object like
// {"coordinator:task_board": [...], "coordinator:iteration": 1}.
func parseTaskBoard(raw string) ([]Task, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	var tasks []Task
	if err := json.Unmarshal([]byte(s), &tasks); err == nil {
		return tasks, nil
	}
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

type workerInfo struct {
	name        string
	description string
	tools       []string
}
