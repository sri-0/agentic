package roster

import (
	"reflect"
	"testing"
)

func TestReadOnlyPermissionsReproducesLegacyDenySet(t *testing.T) {
	// The old explore/plan/verification packages blocked exactly these five.
	legacyBlocked := map[string]bool{
		"write_database": true,
		"add_memory":     true,
		"update_memory":  true,
		"delete_memory":  true,
		"trigger_alert":  true,
	}
	p := ReadOnlyPermissions()
	all := []string{
		"write_database", "add_memory", "update_memory", "delete_memory", "trigger_alert",
		"search_memories", "list_memories", "query_database", "web_search", "view_skill",
	}
	for _, name := range all {
		got := p.Allowed(name)
		want := !legacyBlocked[name]
		if got != want {
			t.Errorf("Allowed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPermissionsLastMatchWins(t *testing.T) {
	p := Permissions{
		Default: EffectDeny,
		Rules: []Rule{
			{Glob: "*", Effect: EffectAllow},
			{Glob: "write_*", Effect: EffectDeny},
			{Glob: "write_safe", Effect: EffectAllow},
		},
	}
	cases := map[string]bool{
		"read_x":     true,  // allowed by "*"
		"write_db":   false, // denied by "write_*"
		"write_safe": true,  // re-allowed by the later, more specific rule
	}
	for name, want := range cases {
		if got := p.Allowed(name); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPermissionsFilterPreservesOrder(t *testing.T) {
	p := ReadOnlyPermissions()
	in := []string{"query_database", "write_database", "web_search", "add_memory", "view_skill"}
	want := []string{"query_database", "web_search", "view_skill"}
	if got := p.Filter(in); !reflect.DeepEqual(got, want) {
		t.Errorf("Filter() = %v, want %v", got, want)
	}
}
