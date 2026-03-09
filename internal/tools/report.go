package tools

import (
	"encoding/json"
	"fmt"

	"agentic/internal/rag"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ── generate_report ────────────────────────────────────────────────────────

type GenerateReportArgs struct {
	Title        string `json:"title" desc:"Title for the research report"`
	Summary      string `json:"summary" desc:"Executive summary of the research"`
	FindingsJSON string `json:"findings_json" desc:"JSON array of Finding objects from the analyst"`
	DBDataJSON   string `json:"db_data_json,omitempty" desc:"Optional JSON of database results for supporting data"`
}

type GenerateReportResult struct {
	Report rag.ResearchReport `json:"report"`
}

func NewGenerateReportTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "generate_report",
		Description: "Assemble the final research report with numbered citations from document metadata. Deduplicates sources and assigns [N] reference numbers.",
	}, generateReportHandler)
}

func generateReportHandler(_ tool.Context, args GenerateReportArgs) (GenerateReportResult, error) {
	var findings []rag.Finding
	if args.FindingsJSON != "" {
		if err := json.Unmarshal([]byte(args.FindingsJSON), &findings); err != nil {
			return GenerateReportResult{}, fmt.Errorf("parsing findings: %w", err)
		}
	}

	// Build deduplicated citation list from all findings' source refs
	citationMap := map[string]int{} // documentID → ref number
	var citations []rag.Citation

	for _, f := range findings {
		for _, ref := range f.SourceRefs {
			if _, exists := citationMap[ref.DocumentID]; !exists {
				refNum := len(citations) + 1
				citationMap[ref.DocumentID] = refNum
				citations = append(citations, rag.Citation{
					RefNumber: refNum,
					Metadata:  ref,
				})
			}
		}
	}

	// Build sections from findings
	var sections []rag.Section

	if len(findings) > 0 {
		body := ""
		for _, f := range findings {
			refs := ""
			for _, ref := range f.SourceRefs {
				if num, ok := citationMap[ref.DocumentID]; ok {
					refs += fmt.Sprintf("[%d]", num)
				}
			}
			body += fmt.Sprintf("- %s %s\n", f.Claim, refs)
		}
		sections = append(sections, rag.Section{
			Heading:  "Key Findings",
			Body:     body,
			Findings: findings,
		})
	}

	if args.DBDataJSON != "" {
		var dbResults []rag.DatabaseResult
		if json.Unmarshal([]byte(args.DBDataJSON), &dbResults) == nil && len(dbResults) > 0 {
			body := ""
			for _, r := range dbResults {
				body += fmt.Sprintf("Data from %s (%d rows)\n", r.Table, r.RowCount)
			}
			sections = append(sections, rag.Section{
				Heading: "Supporting Data",
				Body:    body,
			})
		}
	}

	if len(citations) > 0 {
		body := ""
		for _, c := range citations {
			body += fmt.Sprintf("[%d] %s, \"%s\", %s (%s, %s)\n",
				c.RefNumber,
				c.Metadata.Author,
				c.Metadata.Source,
				c.Metadata.Date,
				c.Metadata.Classification,
				fmt.Sprintf("%.0f%% confidence", c.Metadata.ConfidenceScore*100),
			)
		}
		sections = append(sections, rag.Section{
			Heading: "Sources",
			Body:    body,
		})
	}

	report := rag.ResearchReport{
		Title:     args.Title,
		Summary:   args.Summary,
		Sections:  sections,
		Citations: citations,
	}

	return GenerateReportResult{Report: report}, nil
}
