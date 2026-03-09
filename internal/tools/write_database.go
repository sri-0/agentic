package tools

import (
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type WriteDBArgs struct {
	Table     string         `json:"table" desc:"Target table name (e.g. 'products', 'users', 'orders')"`
	Operation string         `json:"operation" desc:"One of 'insert', 'update', 'delete'"`
	Data      map[string]any `json:"data" desc:"The record data for insert/update, or filter criteria for delete"`
}

type WriteDBResult struct {
	Success      bool           `json:"success"`
	Status       string         `json:"status,omitempty"`
	Table        string         `json:"table,omitempty"`
	Operation    string         `json:"operation,omitempty"`
	RowsAffected int            `json:"rows_affected,omitempty"`
	Timestamp    string         `json:"timestamp,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
}

// NewWriteDatabaseTool creates the write_database tool with ADK's built-in
// RequireConfirmation for HITL approval.
func NewWriteDatabaseTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:                "write_database",
		Description:         "Write, update, or delete records in the database. Use this tool whenever you need to insert, update, or delete data.",
		RequireConfirmation: true, // ADK handles the confirmation flow
	}, writeDatabaseHandler)
}

// writeDatabaseHandler is the actual business logic — only called after
// confirmation is approved by ADK's built-in mechanism.
func writeDatabaseHandler(_ tool.Context, args WriteDBArgs) (WriteDBResult, error) {
	return WriteDBResult{
		Success:      true,
		Table:        args.Table,
		Operation:    args.Operation,
		RowsAffected: 1,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Data:         args.Data,
	}, nil
}
