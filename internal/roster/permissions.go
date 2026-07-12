// Package roster holds the typed agent-definition model: how an agent type's
// tools are scoped (Permissions) and, in later phases, the registry of agent
// definitions sourced from code, markdown, and YAML.
//
// This package is intentionally dependency-light (stdlib only) so it can be
// imported by config, shared builders, and handlers without import cycles.
package roster

import "path"

// Effect is the result of a permission rule match.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Rule grants or denies tools whose name matches Glob (shell-style, via
// path.Match — e.g. "write_*", "*_memory", "gitlab_*", "*").
type Rule struct {
	Glob   string
	Effect Effect
}

// Permissions is a tool-visibility ruleset evaluated last-match-wins against a
// tool name, falling back to Default. It replaces the per-agent writeTools /
// blockedTools maps that were copy-pasted across the explore/plan/verification
// packages.
type Permissions struct {
	Default Effect // EffectAllow if empty
	Rules   []Rule // evaluated top-to-bottom; the LAST matching rule decides
}

// Allowed reports whether the named tool is permitted under p.
func (p Permissions) Allowed(name string) bool {
	eff := p.Default
	if eff == "" {
		eff = EffectAllow
	}
	for _, r := range p.Rules {
		if ok, _ := path.Match(r.Glob, name); ok {
			eff = r.Effect
		}
	}
	return eff == EffectAllow
}

// Filter returns the subset of names allowed by p, preserving order.
func (p Permissions) Filter(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if p.Allowed(n) {
			out = append(out, n)
		}
	}
	return out
}

// ReadOnlyPermissions is the canonical "no state mutation" ruleset shared by the
// read-only agent types (explore, plan, verification). The globs reproduce the
// exact deny set the old per-package maps used — write_database (write_*),
// add/update/delete_memory (*_memory), and trigger_alert — while deliberately
// leaving the read-only memory tools (search_memories, list_memories, which end
// in *_memories) allowed.
func ReadOnlyPermissions() Permissions {
	return Permissions{
		Default: EffectAllow,
		Rules: []Rule{
			{Glob: "write_*", Effect: EffectDeny},
			{Glob: "*_memory", Effect: EffectDeny},
			{Glob: "trigger_alert", Effect: EffectDeny},
		},
	}
}
