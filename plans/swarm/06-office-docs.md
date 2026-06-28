# Phase 06 — Office-document MCP server

> Agents author pptx/docx/xlsx for report-writing, via MCP (rides Phase 04). No bespoke Go doc code.

Depends on: 04 (MCP client + remote-server config). Standalone Python service.

## Approach (researched)

A **thin custom Python MCP server**, streamable-HTTP, that the backend connects to as an ordinary remote MCP client (Phase 04). Reuses maintained MIT building blocks:

- **docx** → `docxtpl` (`github.com/elapouya/python-docx-template`, Jinja2 over a real Word `.docx` template + JSON context). Best fidelity for branded reports: designers build the template in Word; the agent supplies only a JSON context (loops, conditionals, nested data, full formatting preserved). Low token cost, deterministic.
- **pptx** → adopt/fork `GongRzhe/Office-PowerPoint-MCP-Server` (MIT, `python-pptx`, `.potx`/`.pptx` templates, 25+ layouts, already speaks streamable-HTTP + Docker), pointed at a corporate template.
- **xlsx** → adopt `haris-musa/excel-mcp-server` (MIT, `openpyxl`, best-maintained, HTTP) as-is.

### Critical: file return

These servers default to writing a **local path**, but the Go backend doesn't share the container filesystem. So tools must **upload the generated file to object storage (S3/GCS) and return a URL**. Wrap/patch the adopted servers' tool outputs accordingly.

### Execution / sandboxing

This is **structured tool calls, not arbitrary code** (much lower risk than a code interpreter). Run the server as a container with a scratch volume, constrain the output dir, upload + return a URL. **Avoid a general Python code-execution sandbox** (the flagship `mcp-run-python` was archived because Pyodide couldn't be sandboxed safely) unless agents truly need open-ended scripting.

### Registration

Register the office MCP server in the Phase-04 MCP config (`type: remote`, `url`, static `headers`/API-key auth — no OAuth needed for an internal service). Per-agent availability via the `Permissions` glob (e.g. `office_*: allow` for report-writer agents). Generated files surface to the UI as artifacts/links (existing `data-artifact` part or a link).

## Files

**New:** a separate Python MCP server project (`office-mcp/` repo or sibling dir) — `docxtpl` wrapper tool `render_report(template, context)`, the forked pptx server, the excel server, object-storage upload, corporate templates.
**Modify (agentic):** Phase-04 MCP config to add the office server; object-storage config; `Permissions` for report-writer agents.

## Verification

An agent generates a pptx/docx/xlsx → returns a working download URL with template fidelity (branded docx via `docxtpl`, `.potx` pptx layout, xlsx formulas). The backend reaches the server as a remote MCP client; files land in object storage; the UI shows the artifact/link.
