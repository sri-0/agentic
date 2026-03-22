package swarm

// Task represents a unit of work on the swarm task board.
type Task struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Input  string `json:"input"`
	Status string `json:"status"` // "pending", "running", "done", "failed"
	Result string `json:"result,omitempty"`
}
