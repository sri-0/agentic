package mcp

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
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
	// Office document tools return a bare file URL that the model would happily
	// echo into its reply as a "download here" link. Wrap them so the model
	// receives a URL-free confirmation instead; the real URL is stashed on the
	// tool-call event's StateDelta side-channel (never in the model-visible
	// FunctionResponse) so the stream layer can still surface the artifact card.
	if r.name == officeServerName {
		for i, t := range tools {
			if !isOfficeToolName(t.Name()) {
				continue
			}
			runnable, ok := t.(runnableTool)
			if !ok {
				r.logger.Warn().Str("tool", t.Name()).Str("type", fmt.Sprintf("%T", t)).
					Msg("mcp: office tool does not satisfy runnableTool; URL not hidden")
				continue
			}
			r.logger.Debug().Str("tool", t.Name()).Msg("mcp: wrapped office tool to hide file URL from model")
			tools[i] = &officeTool{runnableTool: runnable, logger: r.logger}
		}
	}
	return tools, nil
}

// officeServerName is the mcp.yaml server name for the office-document server.
const officeServerName = "office"

// OfficeArtifactStatePrefix is the StateDelta key prefix under which the office
// tool decorator stashes a generated document's artifact (url/filename/mime),
// keyed by the tool-call id. The stream layer reads it to emit the artifact card
// without the URL ever reaching the model. Kept out of the model-visible
// FunctionResponse on purpose.
const OfficeArtifactStatePrefix = "office:artifact:"

// officeToolBaseNames is the set of office MCP tool base names that return a bare
// file URL. adk's mcptoolset may namespace them (e.g. "office_create_pptx").
var officeToolBaseNames = map[string]bool{
	"create_pptx":        true,
	"render_report_docx": true,
	"create_xlsx":        true,
}

// officeMIMEByExt maps office document extensions to their MIME types.
var officeMIMEByExt = map[string]string{
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

// isOfficeToolName reports whether name is one of the office document tools,
// tolerating the "office_" namespace prefix adk's mcptoolset may add.
func isOfficeToolName(name string) bool {
	if officeToolBaseNames[name] {
		return true
	}
	if trimmed := strings.TrimPrefix(name, "office_"); trimmed != name {
		return officeToolBaseNames[trimmed]
	}
	return false
}

// runnableTool is the (unexported, internal-to-adk) method set an mcp tool
// exposes so the flow can invoke it: tool.Tool plus Declaration/Run (the
// toolinternal.FunctionTool set) and ProcessRequest (RequestProcessor, which
// packs the tool into the LLM request). We declare it structurally here so the
// decorator can wrap the concrete mcptoolset tool — and forward ALL of these
// methods via embedding — without importing adk's internal packages. Missing
// ProcessRequest would make the flow reject the tool ("does not implement
// RequestProcessor() method").
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	ProcessRequest(ctx tool.Context, req *model.LLMRequest) error
	Run(ctx tool.Context, args any) (map[string]any, error)
}

// officeTool decorates an office MCP tool. It runs the inner tool, captures the
// generated file URL, stashes a fully-formed file artifact on the tool-call
// event's StateDelta (a side-channel the model never sees), and returns a
// URL-free confirmation to the model so no download link ever lands in the
// assistant's reply text.
type officeTool struct {
	runnableTool
	logger zerolog.Logger
}

// ProcessRequest registers THIS decorator (not the inner tool) in the LLM
// request's tool map, so the flow's tool dispatch resolves the function call to
// o.Run — the URL-hiding path — rather than the inner tool's Run. It mirrors
// adk's internal toolutils.PackTool using only exported types (that package is
// internal and cannot be imported). Delegating to the embedded inner would pack
// the inner tool and bypass our Run entirely.
func (o *officeTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	name := o.Name()
	if _, ok := req.Tools[name]; ok {
		return fmt.Errorf("duplicate tool: %q", name)
	}
	req.Tools[name] = o

	decl := o.Declaration()
	if decl == nil {
		return nil
	}
	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	for _, t := range req.Config.Tools {
		if t != nil && t.FunctionDeclarations != nil {
			t.FunctionDeclarations = append(t.FunctionDeclarations, decl)
			return nil
		}
	}
	req.Config.Tools = append(req.Config.Tools, &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{decl},
	})
	return nil
}

func (o *officeTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	resp, err := o.runnableTool.Run(ctx, args)
	if err != nil {
		return resp, err
	}

	url := firstHTTPURL(resp, 0)
	if url == "" {
		// Nothing that looks like a file URL — pass through unchanged.
		return resp, nil
	}

	art := officeFileArtifact(url)
	filename, _ := art["filename"].(string)

	// Stash the artifact on the event side-channel keyed by tool-call id so the
	// stream layer can emit the card. StateDelta is NOT part of the model-visible
	// FunctionResponse, so the URL never reaches the model.
	if actions := ctx.Actions(); actions != nil {
		if actions.StateDelta == nil {
			actions.StateDelta = map[string]any{}
		}
		actions.StateDelta[OfficeArtifactStatePrefix+ctx.FunctionCallID()] = art
	}

	o.logger.Info().Str("tool", o.Name()).Str("filename", filename).
		Msg("mcp: office document created; URL hidden from model, artifact emitted via side-channel")

	// URL-free confirmation for the model.
	return map[string]any{
		"status":   "created",
		"filename": filename,
		"message":  "The file has been created and is shown to the user as a downloadable artifact. Do not output the URL or a download link.",
	}, nil
}

// officeFileArtifact builds a file-artifact map (kind:file) for a generated
// office document URL: id/kind/url/filename/mime/title.
func officeFileArtifact(url string) map[string]any {
	// Derive filename from the URL tail (strip any query/fragment).
	filename := url
	if i := strings.IndexAny(filename, "?#"); i >= 0 {
		filename = filename[:i]
	}
	if i := strings.LastIndexByte(filename, '/'); i >= 0 {
		filename = filename[i+1:]
	}
	mime := "application/octet-stream"
	if i := strings.LastIndexByte(filename, '.'); i >= 0 {
		if m, ok := officeMIMEByExt[strings.ToLower(filename[i:])]; ok {
			mime = m
		}
	}
	id := filename // stable id so re-emits dedupe
	if id == "" {
		id = url
	}
	return map[string]any{
		"id":       id,
		"kind":     "file",
		"url":      url,
		"filename": filename,
		"mime":     mime,
		"title":    filename,
	}
}

// firstHTTPURL walks maps/slices depth-first and returns the first string value
// that looks like an http(s) URL. Depth-bounded to avoid pathological nesting.
// The office MCP tools return the URL wrapped in a varying shape (observed:
// {"output": "<url>"}, also {"output": {"result": "<url>"}}), so rather than
// guess the key we walk the response.
func firstHTTPURL(v any, depth int) string {
	if depth > 8 {
		return ""
	}
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
			return t
		}
	case map[string]any:
		for _, val := range t {
			if u := firstHTTPURL(val, depth+1); u != "" {
				return u
			}
		}
	case []any:
		for _, val := range t {
			if u := firstHTTPURL(val, depth+1); u != "" {
				return u
			}
		}
	}
	return ""
}
