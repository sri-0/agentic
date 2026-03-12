package e2e

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"agentic/pkg/db/opensearch"
)

// seedEmbeddings indexes test embedding documents. Returns cleanup func.
func seedEmbeddings(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()

	docs := []struct {
		id  string
		doc map[string]any
	}{
		{
			id: "seed-embed-001",
			doc: map[string]any{
				"project": "acme", "doc_id": "doc-perf-q4", "chunk_id": 0,
				"title":          "Q4 2024 Business Performance Report",
				"source":         "reports/q4-2024-performance.pdf",
				"author":         "Finance Team",
				"date":           "2025-01-15",
				"classification": "internal",
				"text":           "Revenue exceeded targets by 12%. Enterprise segment grew 24% YoY. Cloud Suite became the top-selling product with $95K in revenue. Total MRR reached $284K with 14,820 active users.",
				"vector":         randomVector(1536, 1),
			},
		},
		{
			id: "seed-embed-002",
			doc: map[string]any{
				"project": "acme", "doc_id": "doc-perf-q4", "chunk_id": 1,
				"title":          "Q4 2024 Business Performance Report",
				"source":         "reports/q4-2024-performance.pdf",
				"author":         "Finance Team",
				"date":           "2025-01-15",
				"classification": "internal",
				"text":           "Customer acquisition cost decreased by 18% while lifetime value increased by 22%. The sales team closed 47 enterprise deals in Q4, up from 31 in Q3.",
				"vector":         randomVector(1536, 2),
			},
		},
		{
			id: "seed-embed-003",
			doc: map[string]any{
				"project": "acme", "doc_id": "doc-competitive", "chunk_id": 0,
				"title":          "Competitive Landscape Analysis 2024",
				"source":         "research/competitive-analysis-2024.pdf",
				"author":         "Strategy Team",
				"date":           "2024-12-20",
				"classification": "confidential",
				"text":           "Market share grew from 19.1% to 23.4%. Three main competitors: Acme Corp (31%), TechCo (18%), NovaSoft (12%). Key differentiator is our AI-first approach and enterprise integrations.",
				"vector":         randomVector(1536, 3),
			},
		},
		{
			id: "seed-embed-004",
			doc: map[string]any{
				"project": "acme", "doc_id": "doc-roadmap", "chunk_id": 0,
				"title":          "Product Roadmap 2025",
				"source":         "product/roadmap-2025.md",
				"author":         "Product Team",
				"date":           "2025-01-10",
				"classification": "internal",
				"text":           "Q1: AI assistant integration with RAG pipeline. Q2: API-first redesign with GraphQL. Q3: Mobile apps for iOS and Android. Q4: Enterprise SSO and SCIM provisioning.",
				"vector":         randomVector(1536, 4),
			},
		},
		{
			id: "seed-embed-005",
			doc: map[string]any{
				"project": "acme", "doc_id": "doc-csat", "chunk_id": 0,
				"title":          "Customer Satisfaction Report Q4 2024",
				"source":         "reports/customer-satisfaction-q4.pdf",
				"author":         "Customer Success",
				"date":           "2025-01-08",
				"classification": "internal",
				"text":           "NPS score improved from 64 to 67. Enterprise customers report 94% satisfaction rate. Top requests: better API documentation, mobile access, and SSO support. Churn rate decreased to 2.1%.",
				"vector":         randomVector(1536, 5),
			},
		},
		{
			id: "seed-embed-006",
			doc: map[string]any{
				"project": "acme", "doc_id": "doc-ai-trends", "chunk_id": 0,
				"title":          "AI Market Trends 2025",
				"source":         "research/ai-market-trends-2025.pdf",
				"author":         "Research Team",
				"date":           "2025-02-01",
				"classification": "public",
				"text":           "The enterprise AI market is projected to reach $150B by 2027. Key trends: agentic AI workflows, RAG-based knowledge systems, and AI-native developer tools. Companies investing in AI see 3x productivity gains.",
				"vector":         randomVector(1536, 6),
			},
		},
		{
			id: "seed-embed-007",
			doc: map[string]any{
				"project": "acme", "doc_id": "doc-security", "chunk_id": 0,
				"title":          "Security Audit Report 2024",
				"source":         "security/audit-2024.pdf",
				"author":         "Security Team",
				"date":           "2024-12-15",
				"classification": "confidential",
				"text":           "Zero critical vulnerabilities found. SOC 2 Type II certification renewed. Implemented zero-trust architecture across all services. Penetration testing revealed 3 medium-severity issues, all remediated within 48 hours.",
				"vector":         randomVector(1536, 7),
			},
		},
		{
			id: "seed-embed-008",
			doc: map[string]any{
				"project": "beta-project", "doc_id": "doc-beta-overview", "chunk_id": 0,
				"title":          "Beta Project Overview",
				"source":         "projects/beta-overview.md",
				"author":         "Engineering",
				"date":           "2025-02-15",
				"classification": "internal",
				"text":           "The beta project focuses on building a next-generation data pipeline using Apache Kafka and Flink for real-time stream processing. Target throughput is 1M events per second with sub-100ms latency.",
				"vector":         randomVector(1536, 8),
			},
		},
	}

	var ids []string
	for _, d := range docs {
		_, err := osClient.IndexDocument(ctx, opensearch.IndexEmbeddings, d.id, d.doc)
		if err != nil {
			t.Fatalf("seed embedding %s: %v", d.id, err)
		}
		ids = append(ids, d.id)
	}

	if err := osClient.Refresh(ctx, opensearch.IndexEmbeddings); err != nil {
		t.Fatalf("refresh embeddings: %v", err)
	}

	t.Logf("seeded %d embedding documents", len(ids))

	return func() {
		for _, id := range ids {
			osClient.DeleteDocument(ctx, opensearch.IndexEmbeddings, id)
		}
	}
}

// seedPrompts indexes test prompt templates. Returns cleanup func.
func seedPrompts(t *testing.T) (map[string]string, func()) {
	t.Helper()
	ctx := context.Background()

	prompts := []struct {
		id  string
		doc map[string]any
	}{
		{
			id: "prompt-analyst",
			doc: map[string]any{
				"name":        "business-analyst",
				"description": "Prompt for business data analysis",
				"template":    "You are a senior business analyst. Analyze data thoroughly, provide quantitative insights, and always cite your sources. Structure your response with clear sections: Summary, Key Findings, and Recommendations.",
				"variables":   []string{},
				"tags":        []string{"analysis", "business"},
				"version":     1,
				"created_at":  "2025-01-01T00:00:00Z",
				"updated_at":  "2025-01-01T00:00:00Z",
			},
		},
		{
			id: "prompt-researcher",
			doc: map[string]any{
				"name":        "research-assistant",
				"description": "Prompt for research tasks with citation requirements",
				"template":    "You are a research assistant. When answering questions, always ground your response in the provided context. Use numbered citations [1], [2], etc. If the context doesn't contain enough information, clearly state what is missing.",
				"variables":   []string{},
				"tags":        []string{"research", "citations"},
				"version":     1,
				"created_at":  "2025-01-05T00:00:00Z",
				"updated_at":  "2025-01-05T00:00:00Z",
			},
		},
		{
			id: "prompt-summarizer",
			doc: map[string]any{
				"name":        "executive-summarizer",
				"description": "Concise executive summary prompt",
				"template":    "You are an executive briefing assistant. Provide concise, actionable summaries. Lead with the most important information. Use bullet points for key metrics. Keep responses under 200 words.",
				"variables":   []string{},
				"tags":        []string{"summary", "executive"},
				"version":     2,
				"created_at":  "2025-01-10T00:00:00Z",
				"updated_at":  "2025-02-01T00:00:00Z",
			},
		},
	}

	idMap := make(map[string]string) // name -> id
	var ids []string
	for _, p := range prompts {
		_, err := osClient.IndexDocument(ctx, opensearch.IndexPrompts, p.id, p.doc)
		if err != nil {
			t.Fatalf("seed prompt %s: %v", p.id, err)
		}
		ids = append(ids, p.id)
		idMap[p.doc["name"].(string)] = p.id
	}

	if err := osClient.Refresh(ctx, opensearch.IndexPrompts); err != nil {
		t.Fatalf("refresh prompts: %v", err)
	}

	t.Logf("seeded %d prompt templates", len(ids))

	return idMap, func() {
		for _, id := range ids {
			osClient.DeleteDocument(ctx, opensearch.IndexPrompts, id)
		}
	}
}

// randomVector generates a deterministic pseudo-random unit vector.
func randomVector(dim int, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	v := make([]float64, dim)
	var norm float64
	for i := range v {
		v[i] = r.NormFloat64()
		norm += v[i] * v[i]
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= norm
	}
	return v
}
