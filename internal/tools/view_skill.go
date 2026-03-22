package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"agentic/pkg/db/opensearch"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type ViewSkillArgs struct {
	Name string `json:"name" desc:"The name of the skill to load (as shown in available_skills)"`
}

type ViewSkillResult struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func NewViewSkillTool(osClient *opensearch.Client) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "view_skill",
		Description: "Load the full instructions for a skill by name. Use this to retrieve detailed guidance for a specific skill listed in available_skills.",
	}, func(_ tool.Context, args ViewSkillArgs) (ViewSkillResult, error) {
		if osClient == nil {
			return ViewSkillResult{Name: args.Name, Content: "skills not available (no opensearch)"}, nil
		}

		query := map[string]any{
			"size": 1,
			"query": map[string]any{
				"term": map[string]any{
					"name": args.Name,
				},
			},
		}

		resp, err := osClient.Search(context.Background(), opensearch.IndexSkills, query)
		if err != nil {
			return ViewSkillResult{Name: args.Name}, fmt.Errorf("failed to search skills: %w", err)
		}

		if len(resp.Hits.Hits) == 0 {
			return ViewSkillResult{
				Name:    args.Name,
				Content: fmt.Sprintf("Skill %q not found. Check the available_skills list for valid skill names.", args.Name),
			}, nil
		}

		var skill struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(resp.Hits.Hits[0].Source, &skill); err != nil {
			return ViewSkillResult{Name: args.Name}, fmt.Errorf("failed to parse skill: %w", err)
		}

		return ViewSkillResult{
			Name:    skill.Name,
			Content: skill.Content,
		}, nil
	})
}
