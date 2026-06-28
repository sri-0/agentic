# Phase 04 — MCP integration + backend-held OAuth tokens

> Agents use MCP servers; the backend holds credentials so background runs work after the browser disconnects.

Depends on: 00 (tools/registry, `Permissions`, identity), 02 (leaf builder used by all agents). Required by: 06. Frontend counterpart: `agentui/plans/swarm/04-mcp-ui.md`.

## Why backend-held tokens

Swarms run in the **background** after the browser closes (Phase 01). So when a swarm calls a GitLab MCP tool, the browser may be gone → the **backend** must hold the credential. The browser can do the OAuth *redirect*, but the token lives server-side. (A client-passthrough-via-header model cannot work for after-disconnect runs.)

## Design

### MCP client integration (adk-go built-in)

Use adk-go's `tool/mcptoolset` (`mcptoolset.New(Config{Client, Transport, ToolFilter, RequireConfirmation})`):
- Transports: `local` (stdio child process) + `remote` streamable-HTTP (fallback SSE).
- Tool namespacing: `server_tool` (e.g. `gitlab_create_issue`).
- `tools/list_changed` → live tool-list refresh.
- Per-agent MCP availability through the **same `Permissions` glob model** (Phase 00): `gitlab_*: allow`. No separate MCP permission system.
- Inject MCP server `instructions` into the system prompt (under `<mcp_instructions>`).
- MCP tools route through the existing HITL confirmation path where flagged (`RequireConfirmation`).

### Backend-held OAuth (opencode pattern, server callback)

opencode runs a localhost callback for a CLI; we adapt to a **server** callback for a web gateway:

1. UI "Connect GitLab" → `POST /v1/mcp/{server}/connect` → backend builds the authorize URL with **PKCE** (+ optional **RFC 7591 dynamic client registration** when `clientId` omitted) and a CSRF `state`; returns the URL.
2. Browser redirects to the provider login.
3. Provider → **backend** `GET /v1/mcp/oauth/callback` (validates `state`/CSRF, 5-min timeout).
4. Backend exchanges `code`→token; stores it **server-side keyed by `(userID, mcp_server)`**, **encrypted at rest** (Redis or OpenSearch), with `refresh_token` handling. A "pending" provider buffers tokens in memory and only commits after the full flow succeeds; cached creds invalidated if the server URL changes.
5. Backend MCP client attaches the token; tokens survive browser close + background runs. On `Unauthorized`, server status → `needs_auth` (surfaced to the UI; prompts re-connect).

### Config (per-MCP-server, discriminated union)

```yaml
mcp:
  gitlab:
    type: remote          # remote | local
    url: https://…/mcp
    oauth: true           # oauth | false (+ static headers/api-key alternative)
    enabled: true
    timeout: 30s
  office:
    type: remote
    url: http://office-mcp:8080/mcp
    headers: { Authorization: "Bearer ${OFFICE_MCP_KEY}" }
```

Lifecycle: connect servers concurrently at startup; statuses `connected|disabled|failed|needs_auth|needs_client_registration`; stdio child-process tree cleanup on shutdown.

## Files

**Add:** `internal/mcp/{manager,oauth_provider,callback,tokenstore}.go` (client manager wrapping `mcptoolset`, PKCE/dynamic-registration OAuth provider, callback handler, encrypted per-(user,server) token store); MCP config schema in `internal/config`.
**Modify:** the leaf builder / `Permissions` to merge MCP toolsets per agent; `internal/server/server.go` (connect + callback routes); `internal/bootstrap/bootstrap.go` (start MCP manager).

## Risks

- Token encryption + refresh; key management.
- `state`/CSRF + 5-min timeout on the callback; reject on mismatch.
- Per-user token isolation (keyed by `userID` from the identity seam).
- Stdio orphan processes on shutdown (walk the child tree, SIGTERM).

## Verification

Connect GitLab → browser redirect → backend callback stores the token. An agent calls a `gitlab_*` tool successfully **after the browser is closed** (background run). `needs_auth` surfaces when the token is missing/expired and a re-connect recovers it. MCP tools respect the per-agent `Permissions` glob.
