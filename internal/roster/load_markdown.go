package roster

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentic/internal/config"

	"gopkg.in/yaml.v3"
)

// markdownFrontmatter mirrors the YAML keys an agent .md file may set. The
// markdown body becomes the system prompt. Unset keys fall through to any
// existing YAML/code definition with the same id.
type markdownFrontmatter struct {
	ID                   string   `yaml:"id"`
	Type                 string   `yaml:"type"`
	Mode                 string   `yaml:"mode"`
	Name                 string   `yaml:"name"`
	Description          string   `yaml:"description"`
	Model                string   `yaml:"model"`
	Provider             string   `yaml:"provider"`
	Tools                []string `yaml:"tools"`
	SubAgents            []string `yaml:"sub_agents"`
	Internal             bool     `yaml:"internal"`
	OutputKey            string   `yaml:"output_key"`
	OutputAgent          string   `yaml:"output_agent"`
	MaxIterations        int      `yaml:"max_iterations"`
	MaxParallelWorkers   int      `yaml:"max_parallel_workers"`
	AllowedSubagents     []string `yaml:"allowed_subagents"`
	ReadOnly             bool     `yaml:"read_only"`
	AppendVerdict        bool     `yaml:"append_verdict"`
	InjectSkillsManifest bool     `yaml:"inject_skills_manifest"`
	MCPServers           []string `yaml:"mcp_servers"`
}

// LoadMarkdownDir parses every *.md agent definition in dir (opencode-style:
// YAML frontmatter + markdown body as the prompt) and merges them into ac,
// overriding any existing entry with the same id. A missing dir is not an error.
func LoadMarkdownDir(ac *config.AgentsConfig, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading agents markdown dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		fm, body, err := splitFrontmatter(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if fm.ID == "" {
			fm.ID = strings.TrimSuffix(e.Name(), ".md")
		}
		merged := config.AgentConfig{
			ID:                   fm.ID,
			Type:                 fm.Type,
			Mode:                 fm.Mode,
			Name:                 fm.Name,
			Description:          fm.Description,
			Model:                fm.Model,
			Provider:             fm.Provider,
			SystemPrompt:         strings.TrimSpace(body),
			Tools:                fm.Tools,
			SubAgents:            fm.SubAgents,
			Internal:             fm.Internal,
			OutputKey:            fm.OutputKey,
			OutputAgent:          fm.OutputAgent,
			MaxIterations:        fm.MaxIterations,
			MaxParallelWorkers:   fm.MaxParallelWorkers,
			AllowedSubagents:     fm.AllowedSubagents,
			ReadOnly:             fm.ReadOnly,
			AppendVerdict:        fm.AppendVerdict,
			InjectSkillsManifest: fm.InjectSkillsManifest,
			MCPServers:           fm.MCPServers,
		}
		upsertAgent(ac, merged)
	}
	return nil
}

// splitFrontmatter separates a leading `---`-delimited YAML block from the body.
func splitFrontmatter(raw []byte) (markdownFrontmatter, string, error) {
	var fm markdownFrontmatter
	s := string(raw)
	if !strings.HasPrefix(s, "---") {
		// No frontmatter: whole file is the prompt body.
		return fm, s, nil
	}
	rest := strings.TrimPrefix(s, "---")
	head, body, found := strings.Cut(rest, "\n---")
	if !found {
		return fm, "", fmt.Errorf("unterminated frontmatter")
	}
	body = strings.TrimPrefix(body, "\n")
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		return fm, "", fmt.Errorf("parsing frontmatter: %w", err)
	}
	return fm, body, nil
}

func upsertAgent(ac *config.AgentsConfig, a config.AgentConfig) {
	for i := range ac.Agents {
		if ac.Agents[i].ID == a.ID {
			ac.Agents[i] = a
			return
		}
	}
	ac.Agents = append(ac.Agents, a)
}
