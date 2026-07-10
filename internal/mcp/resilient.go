package mcp

import (
	"github.com/rs/zerolog"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

// resilientToolset wraps an MCP toolset so that a failure to list tools (the
// server is down, unreachable, or unauthenticated) degrades to an EMPTY tool
// list with a logged warning, instead of propagating the error up through
// llmagent tool extraction and killing the entire agent run.
//
// Before this, one unreachable MCP server referenced by an agent's mcp_servers
// made every run of that agent fail with "failed to extract tools from the tool
// set". Now the agent simply runs without that server's tools.
type resilientToolset struct {
	name   string
	inner  tool.Toolset
	logger zerolog.Logger
}

func (r *resilientToolset) Name() string { return r.inner.Name() }

func (r *resilientToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	tools, err := r.inner.Tools(ctx)
	if err != nil {
		r.logger.Warn().Err(err).Str("server", r.name).
			Msg("mcp: tool listing failed; degrading to no tools for this run")
		return nil, nil
	}
	return tools, nil
}
