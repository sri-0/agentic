package coordinator

import "google.golang.org/adk/agent"

// State keys for coordinator orchestration.
const (
	KeyTaskBoard = "coordinator:task_board" // JSON-encoded []Task
	KeySynthesis = "coordinator:synthesis"  // final synthesized output
	KeyIteration = "coordinator:iteration"  // current loop iteration
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
