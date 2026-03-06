package tools

import (
	"fmt"
	"time"

	"agentic/internal/agent"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type WriteDBArgs struct {
	Table     string         `json:"table" desc:"Target table name (e.g. 'products', 'users', 'orders')"`
	Operation string         `json:"operation" desc:"One of 'insert', 'update', 'delete'"`
	Data      map[string]any `json:"data" desc:"The record data for insert/update, or filter criteria for delete"`
}

type WriteDBResult struct {
	Success              bool           `json:"success"`
	Status               string         `json:"status,omitempty"`
	Table                string         `json:"table,omitempty"`
	Operation            string         `json:"operation,omitempty"`
	RowsAffected         int            `json:"rows_affected,omitempty"`
	Timestamp            string         `json:"timestamp,omitempty"`
	Data                 map[string]any `json:"data,omitempty"`
	RequiresConfirmation bool           `json:"requires_confirmation,omitempty"`
	Prompt               string         `json:"prompt,omitempty"`
	Details              map[string]any `json:"details,omitempty"`
}

// NewWriteDatabaseTool creates the write_database tool with HITL support.
// The store is used to track pending confirmations and decisions.
func NewWriteDatabaseTool(store *agent.HITLStore) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "write_database",
		Description: "Write, update, or delete records in the database. This operation modifies data " +
			"and requires human approval before executing.",
	}, func(ctx tool.Context, args WriteDBArgs) (WriteDBResult, error) {
		return writeDBHandler(ctx, args, store)
	})
}

func writeDBHandler(ctx tool.Context, args WriteDBArgs, store *agent.HITLStore) (WriteDBResult, error) {
	threadID := getThreadID(ctx)

	// Check if a decision has been made for this thread
	decision := store.GetAndClearDecision(threadID)

	if decision == "" {
		// No decision yet — store pending confirmation and return marker
		store.SetPending(threadID, &agent.PendingConfirmation{
			ToolCallID: getFunctionCallID(ctx),
			ToolName:   "write_database",
			Prompt: fmt.Sprintf(
				"The agent wants to perform a **%s** operation on the **%s** table. Do you want to allow this?",
				args.Operation, args.Table,
			),
			Details: map[string]any{
				"table":     args.Table,
				"operation": args.Operation,
				"data":      args.Data,
			},
		})
		return WriteDBResult{
			RequiresConfirmation: true,
			Prompt: fmt.Sprintf(
				"The agent wants to perform a **%s** operation on the **%s** table. Do you want to allow this?",
				args.Operation, args.Table,
			),
			Details: map[string]any{
				"table":     args.Table,
				"operation": args.Operation,
				"data":      args.Data,
			},
		}, nil
	}

	if decision != "approved" {
		return WriteDBResult{
			Success: false,
			Status:  fmt.Sprintf("Operation %s by user", decision),
		}, nil
	}

	return WriteDBResult{
		Success:      true,
		Table:        args.Table,
		Operation:    args.Operation,
		RowsAffected: 1,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Data:         args.Data,
	}, nil
}

// getThreadID extracts the thread ID from the tool context.
func getThreadID(ctx tool.Context) string {
	// Try to get session ID (which we set to threadID)
	type sessionProvider interface {
		Session() interface{ ID() string }
	}
	if sp, ok := ctx.(sessionProvider); ok {
		return sp.Session().ID()
	}
	// Fallback: context value
	if tid, ok := ctx.Value(threadIDKey).(string); ok {
		return tid
	}
	return "unknown"
}

// getFunctionCallID extracts the function call ID from tool context.
func getFunctionCallID(ctx tool.Context) string {
	return ctx.FunctionCallID()
}

type contextKeyType string

const threadIDKey contextKeyType = "agentic_thread_id"
