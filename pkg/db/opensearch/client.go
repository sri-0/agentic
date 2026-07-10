package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// Config holds connection settings for OpenSearch.
type Config struct {
	URL      string // e.g. "http://localhost:9200"
	Username string // optional basic auth
	Password string // optional basic auth
}

// Client is a lightweight OpenSearch HTTP client.
type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     zerolog.Logger
	username   string
	password   string
}

// New creates a new OpenSearch client.
func New(cfg Config, logger zerolog.Logger) *Client {
	return &Client{
		baseURL: cfg.URL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger:   logger.With().Str("component", "opensearch").Logger(),
		username: cfg.Username,
		password: cfg.Password,
	}
}

// Ping checks if OpenSearch is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, "GET", "/", nil)
	return err
}

// CreateIndex creates an index with the given mapping. Returns nil if it already exists.
func (c *Client) CreateIndex(ctx context.Context, index string, mapping json.RawMessage) error {
	resp, err := c.doRaw(ctx, "PUT", "/"+index, mapping)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest {
		// Check if "resource_already_exists_exception"
		body, _ := io.ReadAll(resp.Body)
		if bytes.Contains(body, []byte("resource_already_exists_exception")) {
			c.logger.Debug().Str("index", index).Msg("index already exists")
			return nil
		}
		return fmt.Errorf("create index %s: %s", index, string(body))
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create index %s: status %d: %s", index, resp.StatusCode, string(body))
	}

	c.logger.Info().Str("index", index).Msg("index created")
	return nil
}

// IndexDocument indexes a document. If docID is empty, OpenSearch generates one.
func (c *Client) IndexDocument(ctx context.Context, index, docID string, body any) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal document: %w", err)
	}

	var method, path string
	if docID != "" {
		method = "PUT"
		path = fmt.Sprintf("/%s/_doc/%s", index, docID)
	} else {
		method = "POST"
		path = fmt.Sprintf("/%s/_doc", index)
	}

	respBody, err := c.do(ctx, method, path, data)
	if err != nil {
		return "", err
	}

	var result struct {
		ID string `json:"_id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse index response: %w", err)
	}
	return result.ID, nil
}

// CreateDocumentIfAbsent indexes a document only if one with docID does not
// already exist (OpenSearch op_type=create). If the document already exists,
// OpenSearch returns 409 Conflict which is treated as a harmless no-op
// (created reports false, err is nil). This is idempotent: the first caller
// wins and later callers never clobber the existing document. docID is required.
func (c *Client) CreateDocumentIfAbsent(ctx context.Context, index, docID string, body any) (created bool, err error) {
	data, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("marshal document: %w", err)
	}

	path := fmt.Sprintf("/%s/_doc/%s?op_type=create", index, docID)
	resp, err := c.doRaw(ctx, "PUT", path, data)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Document already exists — harmless no-op.
		io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("opensearch create %s/%s: status %d: %s", index, docID, resp.StatusCode, string(respBody))
	}
	io.Copy(io.Discard, resp.Body)
	return true, nil
}

// GetDocument retrieves a document by ID.
func (c *Client) GetDocument(ctx context.Context, index, docID string) (*Hit, error) {
	path := fmt.Sprintf("/%s/_doc/%s", index, docID)
	respBody, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		ID     string          `json:"_id"`
		Found  bool            `json:"found"`
		Source json.RawMessage `json:"_source"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse get response: %w", err)
	}
	if !result.Found {
		return nil, fmt.Errorf("document %s not found in %s", docID, index)
	}
	return &Hit{
		ID:     result.ID,
		Score:  1.0,
		Source: result.Source,
	}, nil
}

// UpdateDocument partially updates a document.
func (c *Client) UpdateDocument(ctx context.Context, index, docID string, doc any) error {
	body := map[string]any{"doc": doc}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal update: %w", err)
	}

	path := fmt.Sprintf("/%s/_update/%s", index, docID)
	_, err = c.do(ctx, "POST", path, data)
	return err
}

// DeleteDocument deletes a document by ID.
func (c *Client) DeleteDocument(ctx context.Context, index, docID string) error {
	path := fmt.Sprintf("/%s/_doc/%s", index, docID)
	_, err := c.do(ctx, "DELETE", path, nil)
	return err
}

// Search performs a query against an index.
func (c *Client) Search(ctx context.Context, index string, query any) (*SearchResponse, error) {
	data, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}

	path := fmt.Sprintf("/%s/_search", index)
	respBody, err := c.do(ctx, "POST", path, data)
	if err != nil {
		return nil, err
	}

	var result SearchResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse search response: %w", err)
	}
	return &result, nil
}

// KNNSearch performs a k-nearest-neighbors vector search.
func (c *Client) KNNSearch(ctx context.Context, index, field string, vector []float64, k int, filters map[string]string) (*SearchResponse, error) {
	knnQuery := map[string]any{
		"size": k,
		"query": map[string]any{
			"knn": map[string]any{
				field: map[string]any{
					"vector": vector,
					"k":      k,
				},
			},
		},
	}

	// Add filters if provided
	if len(filters) > 0 {
		var filterClauses []map[string]any
		for key, val := range filters {
			filterClauses = append(filterClauses, map[string]any{
				"term": map[string]any{key: val},
			})
		}
		knnQuery["query"] = map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"knn": map[string]any{
							field: map[string]any{
								"vector": vector,
								"k":      k,
							},
						},
					},
				},
				"filter": filterClauses,
			},
		}
	}

	return c.Search(ctx, index, knnQuery)
}

// DeleteByQuery deletes all documents matching the given query.
func (c *Client) DeleteByQuery(ctx context.Context, index string, query any) error {
	data, err := json.Marshal(query)
	if err != nil {
		return fmt.Errorf("marshal query: %w", err)
	}
	path := fmt.Sprintf("/%s/_delete_by_query", index)
	_, err = c.do(ctx, "POST", path, data)
	return err
}

// Refresh forces a refresh on an index (useful for tests).
func (c *Client) Refresh(ctx context.Context, index string) error {
	_, err := c.do(ctx, "POST", fmt.Sprintf("/%s/_refresh", index), nil)
	return err
}

// DeleteIndex deletes an index (useful for tests/cleanup).
func (c *Client) DeleteIndex(ctx context.Context, index string) error {
	resp, err := c.doRaw(ctx, "DELETE", "/"+index, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete index %s: status %d: %s", index, resp.StatusCode, string(body))
	}
	return nil
}

// do sends an HTTP request and returns the response body.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	resp, err := c.doRaw(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opensearch %s %s: status %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// doRaw sends an HTTP request and returns the raw response (caller must close body).
func (c *Client) doRaw(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	return c.httpClient.Do(req)
}
