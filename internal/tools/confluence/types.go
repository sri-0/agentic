package confluence

// SearchResponse holds the result of a CQL search.
type SearchResponse struct {
	TotalSize int            `json:"total_size"`
	Results   []SearchResult `json:"results"`
}

// SearchResult is a single search hit.
type SearchResult struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	SpaceKey string `json:"space_key"`
	Excerpt  string `json:"excerpt"`
	Version  int    `json:"version"`
	URL      string `json:"url"`
}

// Page holds the full content of a Confluence page.
type Page struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	SpaceKey  string   `json:"space_key"`
	BodyHTML  string   `json:"body_html"`
	Version   int      `json:"version"`
	Ancestors []string `json:"ancestors"`
	URL       string   `json:"url"`
}
