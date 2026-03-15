package types

type HealthResponse struct {
	Status string   `json:"status" jsonschema_description:"Server status"`
	Agents []string `json:"agents" jsonschema_description:"Loaded agent IDs"`
	Tools  []string `json:"tools" jsonschema_description:"Available tool names"`
}

type ErrorResponse struct {
	Error string `json:"error" jsonschema_description:"Error message"`
}

type DeleteResponse struct {
	Deleted bool `json:"deleted" jsonschema_description:"Whether the resource was deleted"`
}
