package types

type Prompt struct {
	ID          string   `json:"id,omitempty" jsonschema_description:"Prompt ID"`
	Name        string   `json:"name" jsonschema_description:"Prompt name"`
	Description string   `json:"description" jsonschema_description:"Prompt description"`
	Template    string   `json:"template" jsonschema_description:"Template with {{variable}} placeholders"`
	Variables   []string `json:"variables" jsonschema_description:"Template variable names"`
	Tags        []string `json:"tags" jsonschema_description:"Prompt tags"`
	Version     int      `json:"version" jsonschema_description:"Version number"`
	CreatedAt   string   `json:"created_at,omitempty" jsonschema_description:"ISO 8601 timestamp"`
	UpdatedAt   string   `json:"updated_at,omitempty" jsonschema_description:"ISO 8601 timestamp"`
}

type PromptInput struct {
	Name        string   `json:"name" jsonschema:"required" jsonschema_description:"Prompt name"`
	Description string   `json:"description,omitempty" jsonschema_description:"Prompt description"`
	Template    string   `json:"template" jsonschema:"required" jsonschema_description:"Template body"`
	Variables   []string `json:"variables,omitempty" jsonschema_description:"Template variable names"`
	Tags        []string `json:"tags,omitempty" jsonschema_description:"Prompt tags"`
}
