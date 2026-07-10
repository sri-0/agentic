package roster

import "agentic/internal/config"

// Mode controls where an agent can be used.
type Mode string

const (
	ModePrimary  Mode = "primary"  // user-selectable top-level agent
	ModeSubagent Mode = "subagent" // only spawnable via the task tool
	ModeAll      Mode = "all"      // both
)

// Definition is the typed view of one agent type in the roster. In this phase it
// is derived from a config.AgentConfig (the YAML DTO) so the rest of the system
// gains a typed registry without a destructive rewrite; later sourcing layers
// (markdown frontmatter, Go built-ins) feed the same AgentsConfig.
type Definition struct {
	Name        string // stable id (AgentConfig.ID)
	DisplayName string // AgentConfig.Name
	Description string
	Mode        Mode
	Model       string
	Provider    string
	Tools       []string
	Permissions Permissions // resolved tool ruleset (read-only agents, etc.)

	ReadOnly             bool
	AppendVerdict        bool
	InjectSkillsManifest bool
	CanDispatch          bool // may be granted the task tool (orchestrator types)

	Source string // provenance: "yaml" | "<file>.md" | "code"

	cfg *config.AgentConfig // back-ref so orchestrator builders keep working
}

// Config returns the underlying AgentConfig (for builders that still consume it).
func (d *Definition) Config() *config.AgentConfig { return d.cfg }

// dispatchTypes are the orchestrator agent types that can spawn sub-agents.
var dispatchTypes = map[string]bool{"coordinator": true, "swarm": true}

// fromAgentConfig derives a Definition from a YAML AgentConfig, applying the
// same type-driven defaults the builder uses (read-only, verdict, skills).
func fromAgentConfig(ac *config.AgentConfig) *Definition {
	mode := ModePrimary
	if ac.Internal {
		mode = ModeSubagent
	}
	d := &Definition{
		Name:                 ac.ID,
		DisplayName:          ac.Name,
		Description:          ac.Description,
		Mode:                 mode,
		Model:                ac.Model,
		Provider:             ac.Provider,
		Tools:                ac.Tools,
		ReadOnly:             ac.ReadOnly,
		AppendVerdict:        ac.AppendVerdict,
		InjectSkillsManifest: ac.InjectSkillsManifest,
		CanDispatch:          dispatchTypes[ac.Type],
		Source:               "yaml",
		cfg:                  ac,
	}
	switch ac.Type {
	case "explore", "plan":
		d.ReadOnly = true
	case "verification":
		d.ReadOnly = true
		d.AppendVerdict = true
	case "codeguide", "basic", "":
		d.InjectSkillsManifest = true
	}
	if d.ReadOnly {
		d.Permissions = ReadOnlyPermissions()
	}
	return d
}
