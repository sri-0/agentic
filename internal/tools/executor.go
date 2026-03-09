package tools

import (
	"encoding/json"
	"fmt"

	"agentic/internal/agent"
)

// Executor can call tools by name, outside the ADK framework.
type Executor struct {
	hitlStore *agent.HITLStore
}

func NewExecutor(hitlStore *agent.HITLStore) *Executor {
	return &Executor{hitlStore: hitlStore}
}

// Call executes a tool by name with the given args and returns the result as a map.
// threadID and callID are needed for HITL tools.
func (e *Executor) Call(name string, args map[string]any, threadID, callID string) (map[string]any, error) {
	switch name {
	case "query_database":
		return e.callQueryDatabase(args)
	case "write_database":
		return e.callWriteDatabase(args, threadID, callID)
	case "retrieve_documents":
		return e.callRetrieveDocuments(args)
	case "web_search":
		return e.callWebSearch(args)
	case "calculate":
		return e.callCalculate(args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (e *Executor) callQueryDatabase(args map[string]any) (map[string]any, error) {
	sql, _ := args["sql"].(string)
	result, err := queryDatabaseHandler(nil, QueryDatabaseArgs{SQL: sql})
	if err != nil {
		return nil, err
	}
	return toMap(result)
}

func (e *Executor) callWriteDatabase(args map[string]any, threadID, callID string) (map[string]any, error) {
	table, _ := args["table"].(string)
	operation, _ := args["operation"].(string)
	data, _ := args["data"].(map[string]any)
	result, err := executeWriteDB(WriteDBArgs{
		Table: table, Operation: operation, Data: data,
	}, e.hitlStore, threadID, callID)
	if err != nil {
		return nil, err
	}
	return toMap(result)
}

func (e *Executor) callRetrieveDocuments(args map[string]any) (map[string]any, error) {
	query, _ := args["query"].(string)
	topK := 3
	if v, ok := args["top_k"].(float64); ok {
		topK = int(v)
	}
	result, err := retrieveDocumentsHandler(nil, RetrieveDocumentsArgs{Query: query, TopK: topK})
	if err != nil {
		return nil, err
	}
	return toMap(result)
}

func (e *Executor) callWebSearch(args map[string]any) (map[string]any, error) {
	query, _ := args["query"].(string)
	numResults := 4
	if v, ok := args["num_results"].(float64); ok {
		numResults = int(v)
	}
	result, err := webSearchHandler(nil, WebSearchArgs{Query: query, NumResults: numResults})
	if err != nil {
		return nil, err
	}
	return toMap(result)
}

func (e *Executor) callCalculate(args map[string]any) (map[string]any, error) {
	expression, _ := args["expression"].(string)
	result, err := calculateHandler(nil, CalculateArgs{Expression: expression})
	if err != nil {
		return nil, err
	}
	return toMap(result)
}

// toMap converts a struct to map[string]any via JSON round-trip.
func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(b, &m)
	return m, err
}
