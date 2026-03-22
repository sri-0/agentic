// cmd/seed-confluence creates a sample space and pages in Confluence DC for testing.
//
// Prerequisites:
//   - Confluence running at CONFLUENCE_URL (default http://localhost:8090)
//   - Setup wizard completed (license, admin user)
//   - Admin PAT created and set as CONFLUENCE_PAT
//
// Usage:
//
//	go run cmd/seed-confluence/main.go
//	CONFLUENCE_URL=http://confluence:8090 CONFLUENCE_PAT=xxx go run cmd/seed-confluence/main.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

var (
	baseURL = envOr("CONFLUENCE_URL", "http://localhost:8090")
	pat     = os.Getenv("CONFLUENCE_PAT")
)

func main() {
	if pat == "" {
		fmt.Fprintln(os.Stderr, "CONFLUENCE_PAT is required (create a PAT in Confluence: Profile → Personal Access Tokens)")
		os.Exit(1)
	}

	fmt.Printf("Seeding Confluence at %s\n", baseURL)

	// Create space
	spaceKey := "ENG"
	createSpace(spaceKey, "Engineering", "Engineering team knowledge base")

	// Create pages
	pages := []struct {
		title string
		body  string
	}{
		{
			title: "Kubernetes Deployment Guide",
			body: `<h2>Overview</h2>
<p>This guide covers deploying applications to our Kubernetes clusters.</p>
<h2>Cluster Access</h2>
<p>Request access via the DevOps team. You'll need kubectl configured with the correct context.</p>
<h3>Namespaces</h3>
<ul>
<li><strong>dev</strong> - Development workloads</li>
<li><strong>staging</strong> - Pre-production testing</li>
<li><strong>production</strong> - Live services</li>
</ul>
<h2>Deployment Process</h2>
<ol>
<li>Build container image via CI pipeline</li>
<li>Update Helm values in the deployment repo</li>
<li>Create a PR and get approval</li>
<li>ArgoCD syncs automatically on merge</li>
</ol>
<h2>Rollback</h2>
<p>Use <code>kubectl rollout undo deployment/&lt;name&gt;</code> or revert the Helm values PR.</p>`,
		},
		{
			title: "Incident Response Runbook",
			body: `<h2>Severity Levels</h2>
<table>
<tr><th>Level</th><th>Description</th><th>Response Time</th></tr>
<tr><td>SEV1</td><td>Complete service outage</td><td>15 minutes</td></tr>
<tr><td>SEV2</td><td>Degraded service for &gt;50% of users</td><td>30 minutes</td></tr>
<tr><td>SEV3</td><td>Minor feature degradation</td><td>4 hours</td></tr>
<tr><td>SEV4</td><td>Non-urgent issue</td><td>Next business day</td></tr>
</table>
<h2>Incident Commander Checklist</h2>
<ol>
<li>Acknowledge the alert in PagerDuty</li>
<li>Open a war room in Slack #incidents</li>
<li>Assess blast radius and communicate status</li>
<li>Coordinate fix and verify resolution</li>
<li>Schedule post-mortem within 48 hours</li>
</ol>
<h2>Escalation Path</h2>
<p>On-call engineer → Team lead → Engineering manager → VP Engineering</p>`,
		},
		{
			title: "Architecture Decision Records",
			body: `<h2>ADR Process</h2>
<p>We use Architecture Decision Records to document significant technical decisions.</p>
<h2>ADR-001: Use OpenSearch for Document Storage</h2>
<p><strong>Status:</strong> Accepted</p>
<p><strong>Context:</strong> We need a scalable document store with full-text search and vector search capabilities.</p>
<p><strong>Decision:</strong> Use OpenSearch 2.x with the k-NN plugin for hybrid search.</p>
<p><strong>Consequences:</strong> Requires operational expertise for cluster management. Provides excellent search performance.</p>
<h2>ADR-002: Agent Orchestration with ADK-Go</h2>
<p><strong>Status:</strong> Accepted</p>
<p><strong>Context:</strong> Need a framework for building multi-agent AI pipelines with tool calling.</p>
<p><strong>Decision:</strong> Use Google's ADK-Go framework for agent orchestration.</p>
<p><strong>Consequences:</strong> Provides sequential, parallel, and loop agent patterns out of the box.</p>`,
		},
		{
			title: "Onboarding Checklist",
			body: `<h2>First Week</h2>
<ul>
<li>Set up development environment (Go 1.22+, Docker, kubectl)</li>
<li>Clone the monorepo and run <code>make dev</code></li>
<li>Complete security training</li>
<li>Meet your team and buddy</li>
</ul>
<h2>First Month</h2>
<ul>
<li>Complete first code review</li>
<li>Ship first feature or bug fix</li>
<li>Shadow an on-call rotation</li>
<li>Read the Architecture Decision Records</li>
</ul>
<h2>Access Requests</h2>
<p>Submit access requests through ServiceNow for: GitHub, AWS Console, Kubernetes, Confluence, Jira.</p>`,
		},
		{
			title: "API Design Standards",
			body: `<h2>REST API Conventions</h2>
<h3>Naming</h3>
<ul>
<li>Use lowercase kebab-case for paths: <code>/v1/chat-completions</code></li>
<li>Use plural nouns for collections: <code>/v1/threads</code>, <code>/v1/prompts</code></li>
<li>Use path parameters for resource IDs: <code>/v1/threads/{id}</code></li>
</ul>
<h3>HTTP Methods</h3>
<ul>
<li><strong>GET</strong> - Read (idempotent)</li>
<li><strong>POST</strong> - Create</li>
<li><strong>PUT</strong> - Full update</li>
<li><strong>PATCH</strong> - Partial update</li>
<li><strong>DELETE</strong> - Remove</li>
</ul>
<h3>Response Format</h3>
<p>All responses are JSON. Errors use: <code>{"error": {"message": "...", "code": "..."}}</code></p>
<h3>Versioning</h3>
<p>API version in URL path: <code>/v1/</code>, <code>/v2/</code>. No breaking changes within a version.</p>`,
		},
	}

	var parentID string
	for i, p := range pages {
		id := createPage(spaceKey, p.title, p.body, "")
		if i == 0 {
			parentID = id
		}
		fmt.Printf("  Created page: %s (id=%s)\n", p.title, id)
	}

	// Create a child page under the first page
	if parentID != "" {
		childID := createPage(spaceKey, "Helm Chart Reference", `<h2>Chart Structure</h2>
<p>Our Helm charts follow the standard layout:</p>
<pre>
chart/
  Chart.yaml
  values.yaml
  templates/
    deployment.yaml
    service.yaml
    ingress.yaml
</pre>
<h2>Common Values</h2>
<p>Override in your environment-specific values file:</p>
<ul>
<li><code>replicaCount</code> - Number of pods (default: 2)</li>
<li><code>image.tag</code> - Container image tag</li>
<li><code>resources.limits.memory</code> - Memory limit</li>
</ul>`, parentID)
		fmt.Printf("  Created child page: Helm Chart Reference (id=%s, parent=%s)\n", childID, parentID)
	}

	fmt.Println("Done!")
}

func createSpace(key, name, description string) {
	body := map[string]any{
		"key":  key,
		"name": name,
		"description": map[string]any{
			"plain": map[string]any{
				"value":          description,
				"representation": "plain",
			},
		},
	}
	data, _ := json.Marshal(body)

	resp, err := doRequest("POST", "/rest/api/space", data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: create space %s: %v (may already exist)\n", key, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		fmt.Printf("  Created space: %s (%s)\n", name, key)
	} else {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Warning: create space %s: %d %s\n", key, resp.StatusCode, string(respBody))
	}
}

func createPage(spaceKey, title, bodyHTML, parentID string) string {
	page := map[string]any{
		"type":  "page",
		"title": title,
		"space": map[string]any{"key": spaceKey},
		"body": map[string]any{
			"storage": map[string]any{
				"value":          bodyHTML,
				"representation": "storage",
			},
		},
	}
	if parentID != "" {
		page["ancestors"] = []map[string]any{{"id": parentID}}
	}

	data, _ := json.Marshal(page)
	resp, err := doRequest("POST", "/rest/api/content", data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating page %q: %v\n", title, err)
		return ""
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "Error creating page %q: %d %s\n", title, resp.StatusCode, string(respBody))
		return ""
	}

	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(respBody, &result)
	return result.ID
}

func doRequest(method, path string, body []byte) (*http.Response, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if pat != "" {
		req.Header.Set("Authorization", "Bearer "+pat)
	}
	return client.Do(req)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
