package tools

import (
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ── classify_incident ──────────────────────────────────────────────────────

type ClassifyIncidentArgs struct {
	Description string   `json:"description" desc:"Description of the incident to classify"`
	Keywords    []string `json:"keywords" desc:"Topics of interest to match against"`
}

type ClassifyIncidentResult struct {
	MatchedKeywords    []string `json:"matched_keywords"`
	SeveritySuggestion string   `json:"severity_suggestion"`
	Category           string   `json:"category"`
	IsRelated          bool     `json:"is_related"`
}

func NewClassifyIncidentTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "classify_incident",
		Description: "Classify an incident by matching against keywords and topics of interest. Returns matched keywords, suggested severity, category, and whether it is related.",
	}, classifyIncidentHandler)
}

func classifyIncidentHandler(_ tool.Context, args ClassifyIncidentArgs) (ClassifyIncidentResult, error) {
	lower := strings.ToLower(args.Description)

	var matched []string
	for _, kw := range args.Keywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			matched = append(matched, kw)
		}
	}

	// Determine severity based on keyword density
	severity := "low"
	if len(matched) >= 3 {
		severity = "critical"
	} else if len(matched) >= 2 {
		severity = "high"
	} else if len(matched) >= 1 {
		severity = "medium"
	}

	// Determine category from content
	category := "general"
	switch {
	case strings.Contains(lower, "security") || strings.Contains(lower, "breach") || strings.Contains(lower, "unauthorized"):
		category = "security"
	case strings.Contains(lower, "outage") || strings.Contains(lower, "down") || strings.Contains(lower, "unavailable"):
		category = "availability"
	case strings.Contains(lower, "performance") || strings.Contains(lower, "slow") || strings.Contains(lower, "latency"):
		category = "performance"
	case strings.Contains(lower, "data") || strings.Contains(lower, "corruption") || strings.Contains(lower, "loss"):
		category = "data_integrity"
	}

	return ClassifyIncidentResult{
		MatchedKeywords:    matched,
		SeveritySuggestion: severity,
		Category:           category,
		IsRelated:          len(matched) > 0,
	}, nil
}

// ── get_incident_context ───────────────────────────────────────────────────

type GetIncidentContextArgs struct {
	IncidentType string `json:"incident_type" desc:"Type of incident to find similar past incidents for"`
}

type PastIncident struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Category   string `json:"category"`
	Severity   string `json:"severity"`
	Resolution string `json:"resolution"`
	Date       string `json:"date"`
}

type GetIncidentContextResult struct {
	IncidentType string         `json:"incident_type"`
	Similar      []PastIncident `json:"similar_incidents"`
}

func NewGetIncidentContextTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "get_incident_context",
		Description: "Retrieve similar past incidents with resolution information for context.",
	}, getIncidentContextHandler)
}

func getIncidentContextHandler(_ tool.Context, args GetIncidentContextArgs) (GetIncidentContextResult, error) {
	// Mock past incidents by category
	pastIncidents := map[string][]PastIncident{
		"security": {
			{ID: "INC-2024-089", Title: "Unauthorized API access attempt", Category: "security", Severity: "high", Resolution: "Rotated API keys, added rate limiting, notified affected users", Date: "2024-11-15"},
			{ID: "INC-2024-052", Title: "Suspected data exfiltration", Category: "security", Severity: "critical", Resolution: "Blocked IP range, forensic analysis, compliance notification", Date: "2024-08-22"},
		},
		"availability": {
			{ID: "INC-2024-101", Title: "Primary database failover", Category: "availability", Severity: "critical", Resolution: "Automated failover to replica, root cause was disk space", Date: "2024-12-03"},
			{ID: "INC-2024-078", Title: "CDN provider outage", Category: "availability", Severity: "high", Resolution: "Switched to backup CDN, filed ticket with provider", Date: "2024-10-09"},
		},
		"performance": {
			{ID: "INC-2024-095", Title: "API response time degradation", Category: "performance", Severity: "medium", Resolution: "Identified N+1 query, added database index", Date: "2024-11-28"},
			{ID: "INC-2024-067", Title: "Memory leak in worker service", Category: "performance", Severity: "high", Resolution: "Deployed hotfix, added memory monitoring alerts", Date: "2024-09-14"},
		},
		"data_integrity": {
			{ID: "INC-2024-044", Title: "Duplicate records in billing", Category: "data_integrity", Severity: "high", Resolution: "Deduplicated records, added unique constraint, credited affected accounts", Date: "2024-07-19"},
		},
	}

	lower := strings.ToLower(args.IncidentType)
	var similar []PastIncident

	// Try exact match first, then keyword match
	if incidents, ok := pastIncidents[lower]; ok {
		similar = incidents
	} else {
		for cat, incidents := range pastIncidents {
			if strings.Contains(lower, cat) || strings.Contains(cat, lower) {
				similar = append(similar, incidents...)
			}
		}
	}

	// Default to general incidents if nothing matched
	if len(similar) == 0 {
		similar = []PastIncident{
			{ID: "INC-2024-100", Title: "General system issue", Category: "general", Severity: "medium", Resolution: "Investigated and resolved by on-call team", Date: "2024-12-01"},
		}
	}

	return GetIncidentContextResult{
		IncidentType: args.IncidentType,
		Similar:      similar,
	}, nil
}
