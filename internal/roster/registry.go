package roster

import (
	"fmt"
	"sort"
	"strings"

	"agentic/internal/config"
)

// Registry is the typed catalogue of agent definitions. It is the single source
// the coordinator's task tool and GET /v1/agents read from. Built from the YAML
// AgentsConfig today; additional sources (markdown, code) merge into the same
// underlying AgentsConfig before construction.
type Registry struct {
	defs  map[string]*Definition
	order []string
}

// FromAgentsConfig builds a Registry from the loaded YAML roster.
func FromAgentsConfig(ac *config.AgentsConfig) *Registry {
	r := &Registry{defs: make(map[string]*Definition)}
	if ac == nil {
		return r
	}
	for i := range ac.Agents {
		d := fromAgentConfig(&ac.Agents[i])
		r.defs[d.Name] = d
		r.order = append(r.order, d.Name)
	}
	return r
}

// Get returns the definition with the given name.
func (r *Registry) Get(name string) (*Definition, bool) {
	d, ok := r.defs[name]
	return d, ok
}

// Primary returns user-selectable agents (Mode primary|all), in roster order.
func (r *Registry) Primary() []*Definition {
	return r.filter(func(d *Definition) bool { return d.Mode == ModePrimary || d.Mode == ModeAll })
}

// Dispatchable returns agents the task tool may spawn (Mode subagent|all).
func (r *Registry) Dispatchable() []*Definition {
	return r.filter(func(d *Definition) bool { return d.Mode == ModeSubagent || d.Mode == ModeAll })
}

func (r *Registry) filter(keep func(*Definition) bool) []*Definition {
	out := make([]*Definition, 0, len(r.order))
	for _, name := range r.order {
		if d := r.defs[name]; keep(d) {
			out = append(out, d)
		}
	}
	return out
}

// Manifest renders the <available_subagents> block for a coordinator's task
// tool description. If allowed is non-empty it restricts to those names;
// otherwise every dispatchable agent is listed. Output is deterministic.
func (r *Registry) Manifest(allowed []string) string {
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a] = true
	}
	defs := r.Dispatchable()
	var b strings.Builder
	b.WriteString("<available_subagents>\n")
	names := make([]string, 0, len(defs))
	byName := map[string]*Definition{}
	for _, d := range defs {
		if len(allow) > 0 && !allow[d.Name] {
			continue
		}
		names = append(names, d.Name)
		byName[d.Name] = d
	}
	sort.Strings(names)
	for _, n := range names {
		d := byName[n]
		fmt.Fprintf(&b, "- %s: %s\n", d.Name, d.Description)
	}
	b.WriteString("</available_subagents>")
	return b.String()
}
