package swarm

import "google.golang.org/adk/agent"

// State keys for swarm coordination.
const (
	KeyTaskBoard  = "swarm:task_board"  // JSON-encoded []Task
	KeyPlan       = "swarm:plan"        // coordinator's current plan text
	KeySynthesis  = "swarm:synthesis"   // final synthesized output
	KeyIteration  = "swarm:iteration"   // current loop iteration
)

func stateGet(ctx agent.InvocationContext, key string) any {
	v, _ := ctx.Session().State().Get(key)
	return v
}

func stateString(ctx agent.InvocationContext, key string) string {
	v, _ := stateGet(ctx, key).(string)
	return v
}

func stateInt(ctx agent.InvocationContext, key string) int {
	switch v := stateGet(ctx, key).(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}
