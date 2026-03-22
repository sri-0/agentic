package memory

import (
	"fmt"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

const defaultAppName = "agentic"

// Toolset provides agent tools for interacting with long-term memory.
type Toolset struct {
	svc   *Service
	tools []tool.Tool
}

// ToolsetConfig configures the memory toolset.
type ToolsetConfig struct {
	Service *Service
	// AppName scopes memories. Defaults to "agentic".
	AppName string
}

// NewToolset creates the memory toolset with search, add, update, delete, and list tools.
func NewToolset(cfg ToolsetConfig) (*Toolset, error) {
	if cfg.Service == nil {
		return nil, fmt.Errorf("memory Service is required")
	}
	appName := cfg.AppName
	if appName == "" {
		appName = defaultAppName
	}

	ts := &Toolset{svc: cfg.Service}

	searchTool, err := functiontool.New(functiontool.Config{
		Name:        "search_memories",
		Description: "Search long-term memory for relevant information from past conversations. Use this to recall facts, preferences, or context the user previously shared.",
	}, func(ctx tool.Context, args struct {
		Query string `json:"query" desc:"The search query to find relevant memories"`
		Count int    `json:"count" desc:"Number of memories to return (default 5, max 50)"`
	}) (struct {
		Memories []EntryResult `json:"memories"`
		Count    int           `json:"count"`
	}, error) {
		userID := ctx.UserID()
		if userID == "" {
			userID = "anonymous"
		}

		entries, err := ts.svc.Search(ctx, appName, userID, args.Query, args.Count)
		if err != nil {
			return struct {
				Memories []EntryResult `json:"memories"`
				Count    int           `json:"count"`
			}{}, fmt.Errorf("search failed: %w", err)
		}

		results := make([]EntryResult, 0, len(entries))
		for _, e := range entries {
			results = append(results, EntryResult{
				ID:        e.ID,
				Content:   e.Content,
				CreatedAt: e.CreatedAt,
				UpdatedAt: e.UpdatedAt,
			})
		}

		return struct {
			Memories []EntryResult `json:"memories"`
			Count    int           `json:"count"`
		}{Memories: results, Count: len(results)}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("create search_memories tool: %w", err)
	}

	addTool, err := functiontool.New(functiontool.Config{
		Name:        "add_memory",
		Description: "Save important information to long-term memory for future recall. Use this to remember user preferences, important facts, or anything the user explicitly asks you to remember.",
	}, func(ctx tool.Context, args struct {
		Content string `json:"content" desc:"The memory content to store"`
	}) (struct {
		Success bool   `json:"success"`
		ID      string `json:"id"`
		Message string `json:"message"`
	}, error) {
		if args.Content == "" {
			return struct {
				Success bool   `json:"success"`
				ID      string `json:"id"`
				Message string `json:"message"`
			}{Success: false, Message: "content cannot be empty"}, nil
		}

		userID := ctx.UserID()
		if userID == "" {
			userID = "anonymous"
		}

		id, err := ts.svc.Add(ctx, appName, userID, args.Content)
		if err != nil {
			return struct {
				Success bool   `json:"success"`
				ID      string `json:"id"`
				Message string `json:"message"`
			}{Success: false, Message: fmt.Sprintf("failed to save: %v", err)}, nil
		}

		return struct {
			Success bool   `json:"success"`
			ID      string `json:"id"`
			Message string `json:"message"`
		}{Success: true, ID: id, Message: "Memory saved successfully"}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("create add_memory tool: %w", err)
	}

	updateTool, err := functiontool.New(functiontool.Config{
		Name:        "update_memory",
		Description: "Update the content of an existing memory by its ID. Use this to correct outdated information. Requires the memory ID from search_memories results.",
	}, func(ctx tool.Context, args struct {
		MemoryID string `json:"memory_id" desc:"The ID of the memory to update"`
		Content  string `json:"content" desc:"The new content for the memory"`
	}) (struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}, error) {
		if args.MemoryID == "" || args.Content == "" {
			return struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}{Success: false, Message: "memory_id and content are required"}, nil
		}

		userID := ctx.UserID()
		if userID == "" {
			userID = "anonymous"
		}

		if err := ts.svc.Update(ctx, appName, userID, args.MemoryID, args.Content); err != nil {
			return struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}{Success: false, Message: fmt.Sprintf("failed to update: %v", err)}, nil
		}

		return struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}{Success: true, Message: "Memory updated successfully"}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("create update_memory tool: %w", err)
	}

	deleteTool, err := functiontool.New(functiontool.Config{
		Name:        "delete_memory",
		Description: "Delete a memory permanently by its ID. Use this to remove incorrect or outdated information. Requires the memory ID from search_memories results.",
	}, func(ctx tool.Context, args struct {
		MemoryID string `json:"memory_id" desc:"The ID of the memory to delete"`
	}) (struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}, error) {
		if args.MemoryID == "" {
			return struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}{Success: false, Message: "memory_id is required"}, nil
		}

		userID := ctx.UserID()
		if userID == "" {
			userID = "anonymous"
		}

		if err := ts.svc.Delete(ctx, appName, userID, args.MemoryID); err != nil {
			return struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}{Success: false, Message: fmt.Sprintf("failed to delete: %v", err)}, nil
		}

		return struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}{Success: true, Message: "Memory deleted successfully"}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("create delete_memory tool: %w", err)
	}

	listTool, err := functiontool.New(functiontool.Config{
		Name:        "list_memories",
		Description: "List all stored memories for the current user, ordered by most recently updated.",
	}, func(ctx tool.Context, args struct {
		Count int `json:"count" desc:"Maximum number of memories to return (default 50)"`
	}) (struct {
		Memories []EntryResult `json:"memories"`
		Count    int           `json:"count"`
	}, error) {
		userID := ctx.UserID()
		if userID == "" {
			userID = "anonymous"
		}

		entries, err := ts.svc.List(ctx, appName, userID, args.Count)
		if err != nil {
			return struct {
				Memories []EntryResult `json:"memories"`
				Count    int           `json:"count"`
			}{}, fmt.Errorf("list failed: %w", err)
		}

		results := make([]EntryResult, 0, len(entries))
		for _, e := range entries {
			results = append(results, EntryResult{
				ID:        e.ID,
				Content:   e.Content,
				CreatedAt: e.CreatedAt,
				UpdatedAt: e.UpdatedAt,
			})
		}

		return struct {
			Memories []EntryResult `json:"memories"`
			Count    int           `json:"count"`
		}{Memories: results, Count: len(results)}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("create list_memories tool: %w", err)
	}

	ts.tools = []tool.Tool{searchTool, addTool, updateTool, deleteTool, listTool}
	return ts, nil
}

// Tools returns the list of memory tools.
func (ts *Toolset) Tools() []tool.Tool {
	return ts.tools
}

// ToolNames returns the names of all memory tools.
func ToolNames() []string {
	return []string{"search_memories", "add_memory", "update_memory", "delete_memory", "list_memories"}
}

// EntryResult is a memory entry returned to the agent (no vectors).
type EntryResult struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
