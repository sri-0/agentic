# office-mcp — Office-document MCP server (Phase 06)

A thin Python MCP server that generates Word/PowerPoint/Excel files for agent
report-writing. The Go gateway connects to it as a remote MCP client (streamable
HTTP); tools return a **URL** (not a local path) because the backend doesn't share
this process's filesystem.

## Tools

| Tool | Library | Output |
|------|---------|--------|
| `render_report_docx(template, context)` | docxtpl (Jinja2 over a real .docx) | branded Word report URL |
| `create_pptx(title, slides)` | python-pptx | PowerPoint deck URL |
| `create_xlsx(sheet_name, header, rows)` | openpyxl | Excel sheet URL |

## Run

```bash
pip install -r requirements.txt
OFFICE_OUTPUT_DIR=./out OFFICE_BASE_URL=http://localhost:8090/files python server.py
```

Serves MCP on `:8090/mcp`. Put branded `.docx`/`.potx` templates in
`OFFICE_TEMPLATE_DIR` (default `./templates`).

## Register with the gateway

`config/<env>/mcp.yaml`:

```yaml
servers:
  office:
    type: remote
    url: http://localhost:8090/mcp
    enabled: true
```

Then give report-writer agents `mcp_servers: [office]` in their definition.

## Notes / production

- **File serving**: this scaffold writes to `OFFICE_OUTPUT_DIR` and returns
  `OFFICE_BASE_URL/<file>`. In production, upload to S3/GCS and return a signed URL
  (replace `_save_url`).
- **Fidelity**: for branded decks, point `create_pptx` at a corporate `.potx`
  (see GongRzhe/Office-PowerPoint-MCP-Server) and prefer docxtpl templates for docx.
- **Security**: structured tool calls only (no arbitrary code execution); run in a
  container with a constrained output dir.
