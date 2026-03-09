package tools

import (
	"encoding/json"

	"agentic/internal/rag"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ── summarize_documents ────────────────────────────────────────────────────

type SummarizeDocumentsArgs struct {
	DocumentsJSON string `json:"documents_json" desc:"JSON array of Document objects to summarize"`
}

type SummarizeDocumentsResult struct {
	Summary   string   `json:"summary"`
	KeyPoints []string `json:"key_points"`
	DocCount  int      `json:"doc_count"`
}

func NewSummarizeDocumentsTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "summarize_documents",
		Description: "Summarize a set of retrieved documents into key points. Takes JSON array of documents.",
	}, summarizeDocumentsHandler)
}

func summarizeDocumentsHandler(_ tool.Context, args SummarizeDocumentsArgs) (SummarizeDocumentsResult, error) {
	var docs []rag.Document
	if err := json.Unmarshal([]byte(args.DocumentsJSON), &docs); err != nil {
		return SummarizeDocumentsResult{
			Summary: "Failed to parse documents: " + err.Error(),
		}, nil
	}

	var points []string
	for _, doc := range docs {
		content := doc.Content
		if len(content) > 80 {
			content = content[:80]
		}
		points = append(points, doc.Title+": "+content+"...")
	}

	return SummarizeDocumentsResult{
		Summary:   "Analysis of " + string(rune('0'+len(docs))) + " documents covering business performance, competitive landscape, product roadmap, customer satisfaction, and market trends.",
		KeyPoints: points,
		DocCount:  len(docs),
	}, nil
}

// ── extract_findings ───────────────────────────────────────────────────────

type ExtractFindingsArgs struct {
	DocumentsJSON    string `json:"documents_json" desc:"JSON array of Document objects"`
	ResearchQuestion string `json:"research_question" desc:"The research question to extract findings for"`
}

type ExtractFindingsResult struct {
	Question string        `json:"question"`
	Findings []rag.Finding `json:"findings"`
}

func NewExtractFindingsTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "extract_findings",
		Description: "Extract structured findings from documents with source references. Each finding carries the original document metadata for citation.",
	}, extractFindingsHandler)
}

func extractFindingsHandler(_ tool.Context, args ExtractFindingsArgs) (ExtractFindingsResult, error) {
	var docs []rag.Document
	if err := json.Unmarshal([]byte(args.DocumentsJSON), &docs); err != nil {
		return ExtractFindingsResult{
			Question: args.ResearchQuestion,
		}, nil
	}

	var findings []rag.Finding
	for _, doc := range docs {
		findings = append(findings, rag.Finding{
			Claim:      "Key insight from " + doc.Title,
			Evidence:   doc.Content,
			SourceRefs: []rag.DocumentMetadata{doc.Metadata},
			Confidence: doc.Metadata.ConfidenceScore,
		})
	}

	if len(docs) >= 2 {
		var refs []rag.DocumentMetadata
		for _, doc := range docs[:2] {
			refs = append(refs, doc.Metadata)
		}
		findings = append(findings, rag.Finding{
			Claim:      "Cross-document finding: strong correlation between business performance and competitive positioning",
			Evidence:   "Multiple sources indicate positive growth trajectory supported by market share gains.",
			SourceRefs: refs,
			Confidence: 0.88,
		})
	}

	return ExtractFindingsResult{
		Question: args.ResearchQuestion,
		Findings: findings,
	}, nil
}
