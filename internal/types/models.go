package types

type ModelListResponse struct {
	Object string    `json:"object" jsonschema_description:"Always 'list'"`
	Data   []ModelID `json:"data" jsonschema_description:"Available models"`
}

type ModelID struct {
	ID           string `json:"id" jsonschema_description:"Model ID"`
	Object       string `json:"object" jsonschema_description:"Always 'model'"`
	Type         string `json:"type" jsonschema_description:"Model type: llm, agent, embedding"`
	Name         string `json:"name,omitempty" jsonschema_description:"Display name"`
	OwnedBy      string `json:"owned_by" jsonschema_description:"Organization that owns the model"`
	ProviderID   string `json:"provider_id,omitempty" jsonschema_description:"Provider ID"`
	ProviderName string `json:"provider_name,omitempty" jsonschema_description:"Provider display name"`
	Created      int64  `json:"created" jsonschema_description:"Unix timestamp"`
}
