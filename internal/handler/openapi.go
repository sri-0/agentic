package handler

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"agentic/internal/rag"
	"agentic/internal/types"
	oa "agentic/pkg/openapi"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gorilla/mux"
)

//go:embed static/scalar.min.js
var scalarJS []byte

// OpenAPISpec serves a dynamically generated OpenAPI 3.1 spec by walking the
// router's registered routes at request time. Routes that are conditionally
// registered (e.g. when OpenSearch is unavailable) are automatically excluded.
func OpenAPISpec(router *mux.Router, cfg oa.SpecConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		spec := &openapi3.T{
			OpenAPI: "3.1.0",
			Info: &openapi3.Info{
				Title:       cfg.Title,
				Description: cfg.Description,
				Version:     cfg.Version,
			},
			Servers: openapi3.Servers{
				{URL: "/", Description: "Current server"},
			},
			Paths: openapi3.NewPathsWithCapacity(20),
			Components: &openapi3.Components{
				SecuritySchemes: openapi3.SecuritySchemes{
					"BearerAuth": &openapi3.SecuritySchemeRef{
						Value: openapi3.NewSecurityScheme().WithType("http").WithScheme("bearer"),
					},
				},
				Schemas: oa.BuildSchemas(schemaRegistry),
			},
		}

		usedTags := map[string]bool{}

		router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
			tmpl, err := route.GetPathTemplate()
			if err != nil {
				return nil
			}
			methods, err := route.GetMethods()
			if err != nil || len(methods) == 0 {
				return nil
			}
			for _, method := range methods {
				if method == "OPTIONS" {
					continue
				}
				operation := routeMeta(tmpl, method)
				if operation == nil {
					continue
				}
				item := spec.Paths.Value(tmpl)
				if item == nil {
					item = &openapi3.PathItem{}
					spec.Paths.Set(tmpl, item)
				}
				setOperation(item, method, operation)
				if len(operation.Tags) > 0 {
					usedTags[operation.Tags[0]] = true
				}
			}
			return nil
		})

		for _, t := range allTags() {
			if usedTags[t.Name] {
				spec.Tags = append(spec.Tags, t)
			}
		}

		body, _ := json.MarshalIndent(spec, "", "  ")
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

// APIDocs returns a handler serving the Scalar API docs UI.
func APIDocs(title string) http.HandlerFunc {
	return oa.NewAPIDocs(title, "/v1/openapi.json", scalarJS)
}

func setOperation(item *openapi3.PathItem, method string, op *openapi3.Operation) {
	switch strings.ToUpper(method) {
	case "GET":
		item.Get = op
	case "POST":
		item.Post = op
	case "PUT":
		item.Put = op
	case "DELETE":
		item.Delete = op
	case "PATCH":
		item.Patch = op
	}
}

// ---------------------------------------------------------------------------
// Schema registry — maps schema names to Go types for reflection
// ---------------------------------------------------------------------------

var schemaRegistry = map[string]any{
	// Chat
	"ChatCompletionRequest":  types.ChatCompletionRequest{},
	"ChatMessage":            types.ChatMessage{},
	"ChatCompletionResponse": types.ChatCompletionResponse{},

	// Models
	"ModelListResponse": types.ModelListResponse{},
	"ModelID":           types.ModelID{},

	// Embeddings
	"EmbeddingRequest":  types.EmbeddingRequest{},
	"EmbeddingResponse": types.EmbeddingResponse{},

	// Agents
	"ResumeRequest": types.ResumeRequest{},

	// RAG
	"RAGSearchRequest":  rag.SearchRequest{},
	"RAGSearchResponse": rag.SearchResponse{},
	"RAGSearchResult":   rag.SearchResult{},

	// Prompts
	"Prompt":      types.Prompt{},
	"PromptInput": types.PromptInput{},

	// Threads
	"Thread":             types.Thread{},
	"ThreadInput":        types.ThreadInput{},
	"ThreadMessage":      types.ThreadMessage{},
	"ThreadMessageInput": types.ThreadMessageInput{},

	// Common
	"HealthResponse": types.HealthResponse{},
	"ErrorResponse":  types.ErrorResponse{},
	"DeleteResponse": types.DeleteResponse{},
}

// ---------------------------------------------------------------------------
// Route metadata registry
// ---------------------------------------------------------------------------

func routeMeta(path, method string) *openapi3.Operation {
	key := method + " " + path
	if fn, ok := routeRegistry[key]; ok {
		return fn()
	}
	return nil
}

var routeRegistry = map[string]func() *openapi3.Operation{

	"GET /health": func() *openapi3.Operation {
		return oa.NewOp("Health", "Health", "Server health check",
			nil, 200, oa.Ref("HealthResponse"))
	},

	"GET /v1/models": func() *openapi3.Operation {
		return oa.NewOp("ListModels", "Models", "List all available models and agents",
			nil, 200, oa.Ref("ModelListResponse"))
	},

	"POST /v1/chat/completions": func() *openapi3.Operation {
		op := oa.NewOp("CreateChatCompletion", "Chat",
			"Create a chat completion. Supports streaming (SSE) and non-streaming responses. "+
				"When the model is an agent, the request is routed to the agent orchestrator. "+
				"Set `use_rag` to inject retrieved context from the knowledge base before the LLM call.",
			oa.Ref("ChatCompletionRequest"), 200, oa.Ref("ChatCompletionResponse"))
		op.AddResponse(404, openapi3.NewResponse().
			WithDescription("Model not found").
			WithJSONSchemaRef(oa.Ref("ErrorResponse")))
		return op
	},

	"POST /v1/embeddings": func() *openapi3.Operation {
		return oa.NewOp("CreateEmbedding", "Embeddings",
			"Generate embeddings for the given input text. Proxied to the upstream provider.",
			oa.Ref("EmbeddingRequest"), 200, oa.Ref("EmbeddingResponse"))
	},

	"POST /v1/messages": func() *openapi3.Operation {
		return oa.NewOp("CreateMessage", "Chat",
			"Anthropic Messages API proxy. Forwards to the upstream provider.",
			&openapi3.SchemaRef{Value: openapi3.NewObjectSchema()}, 200,
			&openapi3.SchemaRef{Value: openapi3.NewObjectSchema()})
	},

	"POST /v1/agent/resume": func() *openapi3.Operation {
		return oa.NewOp("ResumeAgent", "Agents",
			"Resume an agent after a human-in-the-loop (HITL) tool interrupt. "+
				"The agent pauses when a tool requires approval; send approved/denied/skipped to continue.",
			oa.Ref("ResumeRequest"), 200,
			&openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}})
	},

	"POST /v1/rag/search": func() *openapi3.Operation {
		return oa.NewOp("RAGSearch", "RAG",
			"Search the knowledge base. Supports KNN vector search (if `vector` is provided) "+
				"or text match search. Returns ranked documents with scores.",
			oa.Ref("RAGSearchRequest"), 200, oa.Ref("RAGSearchResponse"))
	},

	"GET /v1/prompts": func() *openapi3.Operation {
		return oa.NewOp("ListPrompts", "Prompts", "List all prompt templates",
			nil, 200, oa.ArrayOf("Prompt"))
	},
	"POST /v1/prompts": func() *openapi3.Operation {
		return oa.NewOp("CreatePrompt", "Prompts", "Create a new prompt template",
			oa.Ref("PromptInput"), 201, oa.Ref("Prompt"))
	},
	"GET /v1/prompts/{id}": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("GetPrompt", "Prompts", "Get a prompt by ID", "id",
			nil, 200, oa.Ref("Prompt"))
	},
	"PUT /v1/prompts/{id}": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("UpdatePrompt", "Prompts", "Update a prompt", "id",
			oa.Ref("PromptInput"), 200, oa.Ref("Prompt"))
	},
	"DELETE /v1/prompts/{id}": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("DeletePrompt", "Prompts", "Delete a prompt", "id",
			nil, 200, oa.Ref("DeleteResponse"))
	},

	"GET /v1/threads": func() *openapi3.Operation {
		return oa.NewOp("ListThreads", "Threads",
			"List conversation threads for the authenticated user. Requires X-User-ID header.",
			nil, 200, oa.ArrayOf("Thread"))
	},
	"POST /v1/threads": func() *openapi3.Operation {
		return oa.NewOp("CreateThread", "Threads", "Create a new conversation thread",
			oa.Ref("ThreadInput"), 201, oa.Ref("Thread"))
	},
	"GET /v1/threads/{id}": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("GetThread", "Threads", "Get a thread by ID", "id",
			nil, 200, oa.Ref("Thread"))
	},
	"PUT /v1/threads/{id}": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("UpdateThread", "Threads", "Update a thread", "id",
			oa.Ref("ThreadInput"), 200, oa.Ref("Thread"))
	},
	"DELETE /v1/threads/{id}": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("DeleteThread", "Threads", "Delete a thread", "id",
			nil, 200, oa.Ref("DeleteResponse"))
	},

	"GET /v1/threads/{id}/messages": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("ListMessages", "Messages", "List messages in a thread", "id",
			nil, 200, oa.ArrayOf("ThreadMessage"))
	},
	"POST /v1/threads/{id}/messages": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("CreateThreadMessage", "Messages", "Add a message to a thread", "id",
			oa.Ref("ThreadMessageInput"), 201, oa.Ref("ThreadMessage"))
	},
	"DELETE /v1/threads/{id}/messages": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("DeleteMessages", "Messages",
			"Delete messages from a thread (by timestamp cutoff)", "id",
			nil, 200, oa.Ref("DeleteResponse"))
	},
	"POST /v1/threads/{id}/messages/bulk": func() *openapi3.Operation {
		return oa.NewOpWithPathParam("BulkCreateMessages", "Messages", "Bulk-add messages to a thread", "id",
			oa.ArrayOf("ThreadMessageInput"),
			200, &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()})
	},
}

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

func allTags() []*openapi3.Tag {
	return []*openapi3.Tag{
		{Name: "Models", Description: "List available models and agents"},
		{Name: "Chat", Description: "Chat completions (streaming & non-streaming)"},
		{Name: "Embeddings", Description: "Generate text embeddings"},
		{Name: "Agents", Description: "Agent resume (HITL approval flow)"},
		{Name: "RAG", Description: "Retrieval-augmented generation search"},
		{Name: "Threads", Description: "Conversation thread management"},
		{Name: "Messages", Description: "Thread message management"},
		{Name: "Prompts", Description: "Prompt template CRUD"},
		{Name: "Health", Description: "Server health check"},
	}
}
