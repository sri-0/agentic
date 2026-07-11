package types

type Thread struct {
	ID        string  `json:"id" jsonschema_description:"Thread ID"`
	UserID    string  `json:"user_id" jsonschema_description:"Owner user ID"`
	Title     *string `json:"title" jsonschema_description:"Thread title"`
	Model     *string `json:"model" jsonschema_description:"Default model"`
	Pinned    bool    `json:"pinned" jsonschema_description:"Whether thread is pinned"`
	PinnedAt  *string `json:"pinned_at" jsonschema_description:"When the thread was pinned"`
	Public    bool    `json:"public" jsonschema_description:"Whether thread is public"`
	ProjectID *string `json:"project_id" jsonschema_description:"Project ID"`
	CreatedAt string  `json:"created_at" jsonschema_description:"ISO 8601 timestamp"`
	UpdatedAt string  `json:"updated_at" jsonschema_description:"ISO 8601 timestamp"`
}

type ThreadInput struct {
	Title  *string `json:"title" jsonschema_description:"Thread title"`
	Model  *string `json:"model" jsonschema_description:"Default model"`
	UserID string  `json:"user_id,omitempty" jsonschema_description:"User ID"`
}

type ThreadMessage struct {
	ID             string `json:"id" jsonschema_description:"Message ID"`
	ThreadID       string `json:"thread_id" jsonschema_description:"Thread ID"`
	UserID         string `json:"user_id,omitempty" jsonschema_description:"Owner user ID"`
	Role           string `json:"role" jsonschema_description:"Message role"`
	Content        string `json:"content" jsonschema_description:"Message content"`
	Parts          any    `json:"parts,omitempty" jsonschema_description:"Structured message parts (tool calls, etc.)"`
	Model          string `json:"model,omitempty" jsonschema_description:"Model that generated the message"`
	AgentID        string `json:"agent_id,omitempty" jsonschema_description:"Agent that produced the message"`
	DurationMs     int64  `json:"duration_ms,omitempty" jsonschema_description:"Wall-clock duration of the turn in ms"`
	MessageGroupID string `json:"message_group_id,omitempty" jsonschema_description:"Group ID for edit history"`
	CreatedAt      string `json:"created_at" jsonschema_description:"ISO 8601 timestamp"`
}

type ThreadMessageInput struct {
	Role           string `json:"role" jsonschema:"required" jsonschema_description:"Message role: user or assistant"`
	Content        string `json:"content" jsonschema:"required" jsonschema_description:"Message content"`
	Parts          any    `json:"parts,omitempty" jsonschema_description:"Structured parts"`
	Model          string `json:"model,omitempty" jsonschema_description:"Model"`
	MessageGroupID string `json:"message_group_id,omitempty" jsonschema_description:"Group ID"`
}
