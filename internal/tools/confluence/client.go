// Package confluence provides an HTTP client for Confluence Data Centre REST API.
package confluence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog"
)

// Config holds connection settings for Confluence Data Centre.
type Config struct {
	BaseURL string // e.g. "http://localhost:8090"
	PAT     string // Personal Access Token for authentication
}

// Client is a lightweight Confluence DC HTTP client.
type Client struct {
	baseURL    string
	pat        string
	httpClient *http.Client
	logger     zerolog.Logger
}

// New creates a new Confluence client.
func New(cfg Config, logger zerolog.Logger) *Client {
	return &Client{
		baseURL: cfg.BaseURL,
		pat:     cfg.PAT,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger.With().Str("component", "confluence").Logger(),
	}
}

// Ping checks if Confluence is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, "GET", "/rest/api/content?limit=1", nil)
	return err
}

// Search performs a CQL search and returns matching content.
func (c *Client) Search(ctx context.Context, cql string, limit int) (*SearchResponse, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	path := fmt.Sprintf("/rest/api/content/search?cql=%s&limit=%d&expand=space,version",
		url.QueryEscape(cql), limit)

	body, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("confluence search: %w", err)
	}

	var raw struct {
		Results []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Title   string `json:"title"`
			Excerpt string `json:"excerpt"`
			Space   struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"space"`
			Version struct {
				Number int `json:"number"`
			} `json:"version"`
			Links struct {
				WebUI string `json:"webui"`
			} `json:"_links"`
		} `json:"results"`
		TotalSize int `json:"totalSize"`
		Links     struct {
			Base string `json:"base"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse search response: %w", err)
	}

	resp := &SearchResponse{
		TotalSize: raw.TotalSize,
	}
	for _, r := range raw.Results {
		u := r.Links.WebUI
		if u != "" && raw.Links.Base != "" {
			u = raw.Links.Base + u
		}
		resp.Results = append(resp.Results, SearchResult{
			ID:       r.ID,
			Title:    r.Title,
			Type:     r.Type,
			SpaceKey: r.Space.Key,
			Excerpt:  r.Excerpt,
			Version:  r.Version.Number,
			URL:      u,
		})
	}
	return resp, nil
}

// GetPage retrieves a single page by ID with its body content.
func (c *Client) GetPage(ctx context.Context, pageID string) (*Page, error) {
	path := fmt.Sprintf("/rest/api/content/%s?expand=body.storage,version,space,ancestors", pageID)

	body, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("confluence get page %s: %w", pageID, err)
	}

	var raw struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Type  string `json:"type"`
		Space struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"space"`
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
		Body struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
		Ancestors []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"ancestors"`
		Links struct {
			Base  string `json:"base"`
			WebUI string `json:"webui"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse page response: %w", err)
	}

	u := raw.Links.WebUI
	if u != "" && raw.Links.Base != "" {
		u = raw.Links.Base + u
	}

	var ancestors []string
	for _, a := range raw.Ancestors {
		ancestors = append(ancestors, a.Title)
	}

	return &Page{
		ID:        raw.ID,
		Title:     raw.Title,
		SpaceKey:  raw.Space.Key,
		BodyHTML:  raw.Body.Storage.Value,
		Version:   raw.Version.Number,
		Ancestors: ancestors,
		URL:       u,
	}, nil
}

// do sends an HTTP request and returns the response body.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.pat != "" {
		req.Header.Set("Authorization", "Bearer "+c.pat)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("confluence %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("confluence auth failed (HTTP 401): check CONFLUENCE_PAT")
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("confluence %s %s: status %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	return respBody, nil
}
