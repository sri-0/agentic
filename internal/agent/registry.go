package agent

import "sort"

// Registry maps agent IDs to Cores.
type Registry struct {
	cores map[string]*Core
}

func NewRegistry() *Registry {
	return &Registry{cores: make(map[string]*Core)}
}

func (r *Registry) Register(id string, core *Core) {
	r.cores[id] = core
}

func (r *Registry) Get(id string) *Core {
	return r.cores[id]
}

func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.cores))
	for id := range r.cores {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
