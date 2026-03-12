package main

import (
	"context"
	"math"
	"math/rand"
	"os"
	"time"

	"agentic/pkg/db/opensearch"

	"github.com/rs/zerolog"
)

func main() {
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Logger()

	url := os.Getenv("OPENSEARCH_URL")
	if url == "" {
		url = "http://localhost:9200"
	}

	client := opensearch.New(opensearch.Config{URL: url}, logger)
	ctx := context.Background()

	if err := client.Ping(ctx); err != nil {
		logger.Fatal().Err(err).Msg("opensearch not reachable")
	}

	if err := opensearch.EnsureIndices(ctx, client); err != nil {
		logger.Fatal().Err(err).Msg("failed to ensure indices")
	}

	seedEmbeddings(ctx, client, logger)
	seedPrompts(ctx, client, logger)
	seedThreads(ctx, client, logger)

	logger.Info().Msg("seed complete")
}

func seedEmbeddings(ctx context.Context, client *opensearch.Client, logger zerolog.Logger) {
	docs := []struct {
		id  string
		doc map[string]any
	}{
		// Q4 Performance Report - 3 chunks
		{id: "perf-q4-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-perf-q4", "chunk_id": 0,
			"title": "Q4 2024 Business Performance Report", "source": "reports/q4-2024-performance.pdf",
			"author": "Finance Team", "date": "2025-01-15", "classification": "internal",
			"text":   "Revenue exceeded targets by 12% in Q4 2024. The enterprise segment grew 24% year-over-year, driven by expansion deals and new logo acquisitions. Cloud Suite became the top-selling product line with $95K in quarterly revenue.",
			"vector": randVec(1536, 1),
		}},
		{id: "perf-q4-002", doc: map[string]any{
			"project": "acme", "doc_id": "doc-perf-q4", "chunk_id": 1,
			"title": "Q4 2024 Business Performance Report", "source": "reports/q4-2024-performance.pdf",
			"author": "Finance Team", "date": "2025-01-15", "classification": "internal",
			"text":   "Total MRR reached $284K with 14,820 active users across all tiers. Customer acquisition cost decreased by 18% while lifetime value increased by 22%. The sales team closed 47 enterprise deals in Q4, up from 31 in Q3.",
			"vector": randVec(1536, 2),
		}},
		{id: "perf-q4-003", doc: map[string]any{
			"project": "acme", "doc_id": "doc-perf-q4", "chunk_id": 2,
			"title": "Q4 2024 Business Performance Report", "source": "reports/q4-2024-performance.pdf",
			"author": "Finance Team", "date": "2025-01-15", "classification": "internal",
			"text":   "Operating margin improved to 23%, up from 19% in Q3. R&D spending remained at 28% of revenue. The company ended Q4 with $12.4M in cash reserves, providing 18 months of runway at current burn rate.",
			"vector": randVec(1536, 3),
		}},

		// Competitive Analysis - 2 chunks
		{id: "competitive-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-competitive", "chunk_id": 0,
			"title": "Competitive Landscape Analysis 2024", "source": "research/competitive-analysis-2024.pdf",
			"author": "Strategy Team", "date": "2024-12-20", "classification": "confidential",
			"text":   "Market share grew from 19.1% to 23.4% in 2024. Three main competitors: Acme Corp holds 31% market share, TechCo at 18%, and NovaSoft at 12%. Our key differentiator is the AI-first approach and deep enterprise integrations.",
			"vector": randVec(1536, 4),
		}},
		{id: "competitive-002", doc: map[string]any{
			"project": "acme", "doc_id": "doc-competitive", "chunk_id": 1,
			"title": "Competitive Landscape Analysis 2024", "source": "research/competitive-analysis-2024.pdf",
			"author": "Strategy Team", "date": "2024-12-20", "classification": "confidential",
			"text":   "TechCo launched a competing AI product in Q3 but lacks enterprise features. NovaSoft is pivoting to vertical solutions. Acme Corp remains the largest threat with their established customer base and recent $50M funding round.",
			"vector": randVec(1536, 5),
		}},

		// Product Roadmap - 2 chunks
		{id: "roadmap-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-roadmap", "chunk_id": 0,
			"title": "Product Roadmap 2025", "source": "product/roadmap-2025.md",
			"author": "Product Team", "date": "2025-01-10", "classification": "internal",
			"text":   "Q1 2025: AI assistant integration with RAG pipeline and vector search. Q2: API-first redesign with GraphQL federation. Q3: Mobile apps for iOS and Android with offline capabilities.",
			"vector": randVec(1536, 6),
		}},
		{id: "roadmap-002", doc: map[string]any{
			"project": "acme", "doc_id": "doc-roadmap", "chunk_id": 1,
			"title": "Product Roadmap 2025", "source": "product/roadmap-2025.md",
			"author": "Product Team", "date": "2025-01-10", "classification": "internal",
			"text":   "Q4 2025: Enterprise SSO with SAML and OIDC, SCIM provisioning, and audit logging. Key dependencies: infrastructure migration to Kubernetes must complete by Q2. Headcount plan: 8 new engineers across platform and AI teams.",
			"vector": randVec(1536, 7),
		}},

		// Customer Satisfaction
		{id: "csat-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-csat", "chunk_id": 0,
			"title": "Customer Satisfaction Report Q4 2024", "source": "reports/customer-satisfaction-q4.pdf",
			"author": "Customer Success", "date": "2025-01-08", "classification": "internal",
			"text":   "NPS score improved from 64 to 67 in Q4. Enterprise customers report 94% satisfaction rate. Top feature requests: better API documentation (42%), mobile access (38%), and SSO support (35%). Churn rate decreased to 2.1% from 2.8%.",
			"vector": randVec(1536, 8),
		}},

		// AI Market Trends
		{id: "ai-trends-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-ai-trends", "chunk_id": 0,
			"title": "AI Market Trends 2025", "source": "research/ai-market-trends-2025.pdf",
			"author": "Research Team", "date": "2025-02-01", "classification": "public",
			"text":   "The enterprise AI market is projected to reach $150B by 2027, growing at 35% CAGR. Key trends: agentic AI workflows replacing traditional automation, RAG-based knowledge systems for enterprise search, and AI-native developer tools gaining mainstream adoption.",
			"vector": randVec(1536, 9),
		}},

		// Security Audit
		{id: "security-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-security", "chunk_id": 0,
			"title": "Security Audit Report 2024", "source": "security/audit-2024.pdf",
			"author": "Security Team", "date": "2024-12-15", "classification": "confidential",
			"text":   "Zero critical vulnerabilities found in annual penetration test. SOC 2 Type II certification renewed. Implemented zero-trust architecture across all services. Three medium-severity issues identified and remediated within 48 hours.",
			"vector": randVec(1536, 10),
		}},

		// Engineering Metrics
		{id: "eng-metrics-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-eng-metrics", "chunk_id": 0,
			"title": "Engineering Team Metrics Q4 2024", "source": "engineering/metrics-q4.pdf",
			"author": "Engineering", "date": "2025-01-12", "classification": "internal",
			"text":   "Sprint velocity increased 15% to an average of 42 story points. Deployment frequency: 12 deploys per week (up from 8). Mean time to recovery: 14 minutes. Test coverage at 87%. Tech debt ratio decreased from 22% to 18%.",
			"vector": randVec(1536, 11),
		}},

		// Hiring Plan
		{id: "hiring-001", doc: map[string]any{
			"project": "acme", "doc_id": "doc-hiring", "chunk_id": 0,
			"title": "2025 Hiring Plan", "source": "hr/hiring-plan-2025.pdf",
			"author": "People Team", "date": "2025-01-20", "classification": "internal",
			"text":   "Planned headcount growth from 82 to 120 employees by end of 2025. Priority roles: 8 engineers (platform, AI, mobile), 4 sales (enterprise AEs), 3 customer success, 2 product managers. Total hiring budget: $4.2M including recruiter fees.",
			"vector": randVec(1536, 12),
		}},

		// Different project - Beta
		{id: "beta-001", doc: map[string]any{
			"project": "beta-project", "doc_id": "doc-beta-arch", "chunk_id": 0,
			"title": "Beta Project Architecture", "source": "projects/beta-architecture.md",
			"author": "Engineering", "date": "2025-02-15", "classification": "internal",
			"text":   "The beta project implements a real-time data pipeline using Apache Kafka for event streaming and Apache Flink for stream processing. Target throughput: 1M events/second with sub-100ms P99 latency. Storage layer uses ClickHouse for analytics.",
			"vector": randVec(1536, 13),
		}},
		{id: "beta-002", doc: map[string]any{
			"project": "beta-project", "doc_id": "doc-beta-status", "chunk_id": 0,
			"title": "Beta Project Status Update", "source": "projects/beta-status-march.md",
			"author": "Engineering", "date": "2025-03-01", "classification": "internal",
			"text":   "Beta project is on track for Q2 delivery. Kafka cluster deployed with 3 brokers. Flink jobs processing 500K events/second in staging. Remaining work: ClickHouse schema optimization, monitoring dashboards, and load testing at full scale.",
			"vector": randVec(1536, 14),
		}},
	}

	count := 0
	for _, d := range docs {
		_, err := client.IndexDocument(ctx, opensearch.IndexEmbeddings, d.id, d.doc)
		if err != nil {
			logger.Error().Err(err).Str("id", d.id).Msg("failed to index embedding")
			continue
		}
		count++
	}

	client.Refresh(ctx, opensearch.IndexEmbeddings)
	logger.Info().Int("count", count).Msg("seeded embeddings")
}

func seedPrompts(ctx context.Context, client *opensearch.Client, logger zerolog.Logger) {
	prompts := []struct {
		id  string
		doc map[string]any
	}{
		{id: "prompt-analyst", doc: map[string]any{
			"name":        "business-analyst",
			"description": "Business data analysis with quantitative insights",
			"template":    "You are a senior business analyst. Analyze data thoroughly, provide quantitative insights, and always cite your sources with reference numbers. Structure your response with: Summary, Key Findings, and Recommendations.",
			"variables":   []string{},
			"tags":        []string{"analysis", "business", "quantitative"},
			"version":     1,
			"created_at":  "2025-01-01T00:00:00Z",
			"updated_at":  "2025-01-01T00:00:00Z",
		}},
		{id: "prompt-researcher", doc: map[string]any{
			"name":        "research-assistant",
			"description": "Research tasks with citation requirements",
			"template":    "You are a research assistant. Ground your response in the provided context. Use numbered citations [1], [2], etc. If the context doesn't contain enough information, clearly state what is missing. Be thorough but concise.",
			"variables":   []string{},
			"tags":        []string{"research", "citations"},
			"version":     1,
			"created_at":  "2025-01-05T00:00:00Z",
			"updated_at":  "2025-01-05T00:00:00Z",
		}},
		{id: "prompt-summarizer", doc: map[string]any{
			"name":        "executive-summarizer",
			"description": "Concise executive briefing summaries",
			"template":    "You are an executive briefing assistant. Provide concise, actionable summaries. Lead with the most important information. Use bullet points for key metrics. Keep responses under 200 words unless asked for detail.",
			"variables":   []string{},
			"tags":        []string{"summary", "executive", "concise"},
			"version":     2,
			"created_at":  "2025-01-10T00:00:00Z",
			"updated_at":  "2025-02-01T00:00:00Z",
		}},
		{id: "prompt-technical", doc: map[string]any{
			"name":        "technical-reviewer",
			"description": "Technical document review and analysis",
			"template":    "You are a senior technical architect. Review the provided information with a focus on architecture decisions, scalability concerns, and technical trade-offs. Highlight risks and suggest improvements. Use specific technical terminology.",
			"variables":   []string{},
			"tags":        []string{"technical", "architecture", "review"},
			"version":     1,
			"created_at":  "2025-02-01T00:00:00Z",
			"updated_at":  "2025-02-01T00:00:00Z",
		}},
		{id: "prompt-competitor", doc: map[string]any{
			"name":        "competitive-intelligence",
			"description": "Competitive analysis and market positioning",
			"template":    "You are a competitive intelligence analyst. Analyze market positioning, identify threats and opportunities, and provide strategic recommendations. Compare strengths and weaknesses objectively. Support claims with data.",
			"variables":   []string{},
			"tags":        []string{"competitive", "strategy", "market"},
			"version":     1,
			"created_at":  "2025-02-10T00:00:00Z",
			"updated_at":  "2025-02-10T00:00:00Z",
		}},
	}

	count := 0
	for _, p := range prompts {
		_, err := client.IndexDocument(ctx, opensearch.IndexPrompts, p.id, p.doc)
		if err != nil {
			logger.Error().Err(err).Str("id", p.id).Msg("failed to index prompt")
			continue
		}
		count++
	}

	client.Refresh(ctx, opensearch.IndexPrompts)
	logger.Info().Int("count", count).Msg("seeded prompts")
}

func seedThreads(ctx context.Context, client *opensearch.Client, logger zerolog.Logger) {
	threads := []struct {
		id  string
		doc map[string]any
	}{
		{id: "thread-001", doc: map[string]any{
			"user_id":    "local-user",
			"title":      "Q4 Performance Review",
			"model":      "test-agent",
			"pinned":     true,
			"pinned_at":  "2025-03-01T00:00:00Z",
			"public":     false,
			"project_id": nil,
			"created_at": "2025-02-15T10:00:00Z",
			"updated_at": "2025-03-01T14:30:00Z",
		}},
		{id: "thread-002", doc: map[string]any{
			"user_id":    "local-user",
			"title":      "Help with Go code",
			"model":      "openai/gpt-4o-mini",
			"pinned":     false,
			"pinned_at":  nil,
			"public":     false,
			"project_id": nil,
			"created_at": "2025-03-05T09:00:00Z",
			"updated_at": "2025-03-05T09:45:00Z",
		}},
		{id: "thread-003", doc: map[string]any{
			"user_id":    "local-user",
			"title":      "Deep research on AI trends",
			"model":      "deep-research",
			"pinned":     false,
			"pinned_at":  nil,
			"public":     true,
			"project_id": nil,
			"created_at": "2025-03-10T08:00:00Z",
			"updated_at": "2025-03-10T10:00:00Z",
		}},
	}

	count := 0
	for _, t := range threads {
		_, err := client.IndexDocument(ctx, opensearch.IndexThreads, t.id, t.doc)
		if err != nil {
			logger.Error().Err(err).Str("id", t.id).Msg("failed to index thread")
			continue
		}
		count++
	}

	// Seed some messages for thread-001
	messages := []struct {
		id  string
		doc map[string]any
	}{
		{id: "msg-001", doc: map[string]any{
			"thread_id":  "thread-001",
			"user_id":    "local-user",
			"role":       "user",
			"content":    "What were our key metrics in Q4?",
			"model":      "",
			"created_at": "2025-02-15T10:00:00Z",
		}},
		{id: "msg-002", doc: map[string]any{
			"thread_id":  "thread-001",
			"role":       "assistant",
			"content":    "Based on the Q4 2024 performance data, here are the key metrics:\n\n- Revenue exceeded targets by 12%\n- MRR reached $284K with 14,820 active users\n- Operating margin improved to 23%\n- 47 enterprise deals closed",
			"model":      "test-agent",
			"created_at": "2025-02-15T10:00:30Z",
		}},
		{id: "msg-003", doc: map[string]any{
			"thread_id":  "thread-001",
			"user_id":    "local-user",
			"role":       "user",
			"content":    "How does that compare to Q3?",
			"model":      "",
			"created_at": "2025-02-15T10:01:00Z",
		}},
		{id: "msg-004", doc: map[string]any{
			"thread_id":  "thread-001",
			"role":       "assistant",
			"content":    "Compared to Q3:\n- Operating margin improved from 19% to 23%\n- Enterprise deals increased from 31 to 47 (52% growth)\n- Customer acquisition cost decreased by 18%\n- Lifetime value increased by 22%",
			"model":      "test-agent",
			"created_at": "2025-02-15T10:01:30Z",
		}},
	}

	msgCount := 0
	for _, m := range messages {
		_, err := client.IndexDocument(ctx, opensearch.IndexMessages, m.id, m.doc)
		if err != nil {
			logger.Error().Err(err).Str("id", m.id).Msg("failed to index message")
			continue
		}
		msgCount++
	}

	client.Refresh(ctx, opensearch.IndexThreads)
	client.Refresh(ctx, opensearch.IndexMessages)
	logger.Info().Int("threads", count).Int("messages", msgCount).Msg("seeded threads")
}

func randVec(dim int, seed int64) []float64 {
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
