# Phase 00 — Foundations: typed registry, leaf consolidation, identity seam

> Substrate for the whole effort. No swarm behaviour change yet — this replaces the `builders` map + flat `AgentConfig` + duplicated leaf builders with one typed registry, one leaf builder, and one permission model, and establishes the identity resolver.

Depends on: nothing. Required by: all later phases.

## Problem (current state, verified)

- `internal/bootstrap/bootstrap.go:45` holds a `builders map[string]agentBuilder` (type→constructor) and `BuildAgentTree` (`:62`). Every agent *type* is a Go package with a `NewAgent`.
- `internal/config/agents.go:14` `AgentConfig` conflates leaf definition, pipeline wiring (`SubAgents`, `OutputAgent`, `MaxIterations`), and runtime override (`overrideModel`). Roster declared in `config/default/agents.yaml` (~671 lines).
- `agents/explore`, `agents/plan`, `agents/verification`, `agents/codeguide` each **re-implement** the leaf builder from `agents/shared/shared.go:BuildLLMAgent` and copy-paste a `writeTools`/`blockedTools` map + `filterReadOnly`/`filterVerificationTools` (three subtly different copies of "deny write tools").
- `agents/coordinator/*` ≈ `agents/swarm/*` line-for-line.

## Design

### 1. `internal/roster` package — typed definitions

```go
package roster

type Mode string // "primary" | "subagent" | "all"

type Definition struct {
    Name        string        // stable id, e.g. "explore" (used by task() + GET /v1/agents)
    DisplayName string        // UI label
    Description string        // advertised to the coordinator task tool + UI card
    Mode        Mode          // primary=user-selectable, subagent=task-only, all=both
    Model       string
    Provider    string
    Prompt      string        // system prompt body
    Tools       Permissions   // tool/permission ruleset (§3)
    Temperature *float64      // nil => provider default
    MaxSteps    int           // per-leaf tool-call discipline cap
    CanDispatch bool          // gated nesting: may this type be granted the task tool? (Phase 02)
    Pipeline    *PipelineSpec // non-nil => static ADK workflow, not a leaf
    InjectSkillsManifest bool // codeguide behaviour, now a flag
    Source      string        // "code" | "<file>.md" | "yaml-override" (provenance)
}

type Registry struct {
    defs  map[string]*Definition
    order []string
}

func (r *Registry) Get(name string) (*Definition, bool)
func (r *Registry) Primary() []*Definition          // Mode primary|all
func (r *Registry) Dispatchable() []*Definition      // Mode subagent|all — what task() can spawn
func (r *Registry) Manifest(allowed []string) string // <available_subagents> block

// Per-request model override, applied at BUILD time (keeps Definition immutable/shareable).
type Overlay struct{ Model, Provider string }
```

`Definition` replaces both `AgentConfig` (leaf fields) and the `builders` map (the `Pipeline` discriminator replaces `Type`→constructor). The per-request override (`AgentConfig.WithModelOverride`, `agents.go:43`) becomes `Overlay` applied in `Construct`, not stored on the definition.

### 2. Sourcing (three layers, last-wins per field)

1. **Go code** — `internal/roster/builtin.go` registers built-ins (`basic`, `explore`, `plan`, `verification`, `guide`, `coordinator`). The floor: a working roster with no config.
2. **Markdown + frontmatter** — `config/<env>/agents/*.md` (opencode-style): YAML frontmatter (`mode`, `model`, `tools`, `temperature`, `can_dispatch`, `description`) + markdown body as `prompt`. Filename stem = `Name`. New `roster.LoadMarkdown(dir)`.
3. **YAML overrides** — migrate existing `config/default/agents.yaml` via `roster.LoadYAMLOverrides`: each non-`internal` `AgentConfig` → a `Definition`; each `internal:true` worker → a `subagent`-mode `Definition`; `type: deep-research|triage` → `PipelineSpec`.

`roster.Build(builtin, md, yaml)` merges by `Name`, last-writer-wins **per field** (an `.md` can override only the model, keeping the Go prompt). Record `Source`.

### 3. Permissions — one model (glob, last-match-wins)

```go
type Permissions struct {
    Default          string      // "allow" | "deny"
    Rules            []PermRule  // evaluated top-to-bottom, LAST match wins
    AllowedSubagents []string    // dispatch-capable types: which subagent_types task() may spawn
}
type PermRule struct{ Glob, Effect string } // Effect: "allow" | "deny"

func (p Permissions) Resolve(deps tools.Deps) ([]tool.Tool, error) // against tools.ToolNames()
```

Subsumes all three copied blocklists. E.g. explore/verification become `Default:"allow"` + `{"write_*","deny"}`, `{"*_memory","deny"}`. `HITLToolNames` (`registry.go:31`) stays orthogonal (approval ≠ visibility). This is the **only** place a tool is filtered out; it also gates the `task` tool (Phase 02).

### 4. One leaf builder + `Construct`

- Generalise `agents/shared/shared.go:BuildLLMAgent` to take `*roster.Definition` (+ `Overlay`, `tools.Deps`). Tool resolution delegates to `Permissions.Resolve`. Keep the centralised tool-discipline appendix (`shared.go:66`) and `shared.BuildSkillsManifest` (gated by `InjectSkillsManifest`).
- `roster.Construct(reg, def, ov, deps) (adkagent.Agent, error)`: leaf if `Pipeline==nil`; else build the static ADK workflow (recursively `Construct` steps, wrap in `sequentialagent`/`parallelagent`/`loopagent`). Replaces `bootstrap.builders` + `BuildAgentTree`.
- **`PipelineSpec`** (for `deepresearch`, `triage`):
  ```go
  type PipelineSpec struct {
      Kind string // "sequential" | "parallel" | "loop"
      Steps []PipelineStep // a Definition name or a nested PipelineSpec
      MaxIterations int     // loop only
  }
  ```
  Encode the documented ADK gotcha **once** in the interpreter: a `loop` step that is *not last* in a `sequential` parent must be lowered to a custom `agent.New` loop (the `deepresearch/agent.go:35-89` workaround), never `loopagent.New`. Reject/lower at load with a clear error.

### 5. Identity seam

Promote `internal/handler/threads.go:getUserID` to a shared `handler.UserID(r) string` (reads `X-User-Id` header, default `"anonymous"`). Single injection point for future SSO/JWT. (Also fixes the `default` vs `anonymous` inconsistency where agent runs hardcode `userID="default"`.)

### 6. `GET /v1/agents`

Rewrite `internal/handler/models.go:Agents`/`buildAgentEntry` to iterate `Registry.Primary()`; add `mode`/`can_dispatch`; `sub_agents` = `Registry.Dispatchable()` filtered by the primary's `AllowedSubagents`. Keeps the UI typed-roster aware.

## Files

**Add:** `internal/roster/{definition,registry,builtin,load_markdown,load_yaml,permissions,construct}.go`; `config/<env>/agents/{explore,plan,verification,guide}.md`.
**Modify:** `agents/shared/shared.go` (generalise builder + delegate to `Permissions`); `internal/bootstrap/bootstrap.go` (build Registry; drop `builders`/`BuildAgentTree`); `internal/handler/models.go`; `internal/handler/chat.go` (override via `Overlay` + `Construct`, replacing `OverrideCoreFunc`/`WithModelOverride`); `internal/config/config.go` (env dir already supported).
**Delete:** `agents/{explore,plan,verification,codeguide}` packages and their `writeTools`/`blockedTools`/`filterReadOnly`/`filterVerificationTools`.
**Keep (this phase):** `agents/{deepresearch,triage,basic}`, `agents/coordinator`/`agents/swarm` (deleted in Phase 02), `internal/agents` (internal system agents, Phase 03).

## Migration / back-compat

Keep the YAML loader so existing `config/default/agents.yaml` deployments work during rollout; deprecate after `.md` definitions land. `GET /v1/agents` shape change (adds `mode`/`can_dispatch`; `sub_agents` now = dispatchable roster) — coordinate with agentui (Phase 07), but additive fields are safe.

## Verification

- `go build ./...` clean.
- `GET /v1/agents` returns the typed roster with `mode`/`can_dispatch`.
- explore/plan/verification/guide run with correct restrictions (explore cannot call `write_*`; verification emits the VERDICT line from its prompt).
- Diff confirms the four packages + duplicated maps are gone, replaced by `.md` + `Permissions`.
- Unit tests: `Permissions.Resolve` (glob + last-match-wins, default allow/deny, `task` gating); `roster.Build` merge precedence; `PipelineSpec` loop-lowering rule.
