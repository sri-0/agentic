# 05 — MCP Auth (static + backend-held OAuth), Office MCP, and the File-Artifact Pipeline

> **Series**: code-migration plan, doc 5. Prereqs: docs 01–04 (baseline layout, config
> system, agent/stream core, session/eventlog). Source repo state: merged `main`
> @ `17b1a87`.
>
> **Audience**: an agent porting these changes into a **diverged fork**. Do not
> blind-copy files. Read the fork's current code first, preserve its local changes,
> and *implement* the behavior described here — including the fixes in
> [Known issues](#known-issues--fix-during-port), which you should build in rather
> than reproduce.

Source commits (for `git show` while porting):

| Commit | What it did |
|---|---|
| `92d412d` | W5: MCP static-header + backend-held OAuth auth, ${ENV} expansion, no-panic config loading, office `/files` serving |
| `0ae0ffd` | W5 follow-up: down MCP server degrades to no-tools instead of killing the run (`resilientToolset`) |
| `e647d53` | PR #18: emit `kind:file` artifacts for office tool results (encoder + eventlog mirrors, stream.go detection) |
| `bab44f3` | PR #19: `officeTool` decorator hides the file URL from the model; artifact travels via StateDelta side-channel |

---

## 1. Purpose and architecture

Two intertwined subsystems land here:

**A. MCP client auth + resilience.** The Go backend (not the browser) is the MCP
client, because swarm runs continue in the background after the browser
disconnects. That forces credentials server-side:

- **Static headers** (API keys): `headers:` in `mcp.yaml`, `${ENV}`-expanded,
  injected into every outgoing HTTP request by a custom `RoundTripper`
  (`internal/mcp/roundtripper.go`). Before this, remote servers were contacted
  **unauthenticated** — the headers key existed in config but was never used.
- **Backend-held OAuth** (the "GitLab design"): the browser only performs the
  redirect dance; the code exchange happens on the gateway and the token is
  stored server-side, AES-256-GCM encrypted, keyed `(userID, server)`. Flow
  pieces: PKCE S256 + single-use CSRF state with 5-min TTL (`oauth.go`),
  RFC 8414 discovery of authorize/token endpoints, RFC 7591 dynamic client
  registration when no `client_id` is configured, refresh-when-expired.
- **No-panic config**: `NewManager` validates each server (`local` needs a
  non-empty `command[0]`, `remote` needs a URL, unknown types rejected) and
  degrades bad entries to `StatusFailed` instead of panicking during bootstrap.
- **Resilient toolset**: an unreachable MCP server used to fail *every run* of
  any agent whose `mcp_servers` referenced it ("failed to extract tools from the
  tool set"). `resilientToolset` catches the listing error, logs, and returns an
  empty tool list so the run proceeds without that server's tools.

**B. Office documents as artifact cards.** `services/office-mcp/server.py`
(Python FastMCP, streamable HTTP on :8090) generates pptx/docx/xlsx and returns
a URL. Three problems were fixed in sequence:

1. The returned URLs 404'd — nothing served the output dir. Fix: a
   `/files/{filename}` route on the *same* Starlette app as `/mcp`, with
   path-traversal guards (`92d412d`).
2. The file only appeared as a raw URL in chat text. Fix: emit a `kind:file`
   artifact data-part so the UI shows a downloadable card (`e647d53`).
3. The model still echoed the raw `localhost:8090/files/...` link. Fix: an
   `officeTool` decorator intercepts the tool result **before the model sees
   it**, stashes a fully-formed artifact on the ADK event's `StateDelta`
   side-channel, and hands the model a URL-free confirmation (`bab44f3`). The
   model *cannot* leak a URL it never received.

---

## 2. OAuth sequence (backend-held, PKCE S256)

```
 Browser                Gateway (Go)                          OAuth Provider          TokenStore
    |                        |                                      |                     |
    |  POST /v1/mcp/{srv}/connect                                   |                     |
    |----------------------->|                                      |                     |
    |                        | resolveEndpoints():                  |                     |
    |                        |   config authorize_url/token_url, or |                     |
    |                        |   GET /.well-known/oauth-authorization-server (RFC 8414)   |
    |                        |------------------------------------->|                     |
    |                        |  no client_id? POST register (RFC 7591, PKCE public client)|
    |                        |------------------------------------->|                     |
    |                        | newPKCE (S256) + state (24B rand)    |                     |
    |                        | pendingStore.put(state, {user,srv,   |                     |
    |                        |   verifier, clientID, redirect})     |  [5-min TTL,        |
    |                        |                                      |   single-use]       |
    |  200 {authorize_url}   |                                      |                     |
    |<-----------------------|                                      |                     |
    |  browser navigates to authorize_url (code_challenge, state)   |                     |
    |--------------------------------------------------------------->                     |
    |            ...user consents...                                |                     |
    |  302 -> GET /v1/mcp/oauth/callback?code=..&state=..           |                     |
    |----------------------->|                                      |                     |
    |                        | pendingStore.take(state)  (CSRF+TTL, consumed)             |
    |                        | POST token endpoint: grant_type=authorization_code,        |
    |                        |   code, code_verifier, redirect_uri, client_id             |
    |                        |------------------------------------->|                     |
    |                        |  {access_token, refresh_token, ...}  |                     |
    |                        |<-------------------------------------|                     |
    |                        | store.Put(userID, server, token)  -- AES-256-GCM --------->|
    |  200 "Connected, close this window"                           |                     |
    |<-----------------------|                                      |                     |

 Later, any MCP tool call (background-safe, browser gone):
    agent run ctx --(WithUserID)*--> transport POST --> authRoundTripper
       --> oauth.TokenFor(user, srv): store.Get, refresh if Expired() --> Authorization: Bearer <tok>

 (*) THE MISSING LINK: WithUserID is never called in the run path today.
     See Known issue (a) — the fork must wire it or skip the OAuth phase.
```

## 3. Office artifact data-flow

```
 model emits FunctionCall create_pptx ──> ADK flow dispatch resolves tool by name
                                              │  (officeTool.ProcessRequest registered the
                                              │   DECORATOR in req.Tools, not the inner tool)
                                              v
                             officeTool.Run (internal/mcp/resilient.go:157)
                                inner mcptoolset tool → office server → {"output": "<url>"}
                                firstHTTPURL(resp) → url
                                officeFileArtifact(url) → {id,kind:file,url,filename,mime,title}
                                ctx.Actions().StateDelta["office:artifact:"+callID] = art
                                return URL-FREE {"status":"created","filename",...} ── to model
                                              │
                       (event side-channel — never in model context)
                                              v
             stream.go event loop: part.FunctionResponse != nil
                officeArtifactFromState(event.Actions.StateDelta, fr.ID)   stream.go:449
                → enc.Artifact(art)
                                              v
             aisdk encoder.Artifact (encoder.go:255) → SSE frame:
                {"type":"data-artifact","id":"deck-ab12cd34ef.pptx",
                 "data":{"id","title","kind":"file","content":"","url","filename","mime"}}
                                              v
             eventlog replay: project.go artifactData (project.go:435) mirrors the
             SAME shape → GET /threads/{id}/messages reconstructs the part
                                              v
             UI renders a downloadable file card in the artifact side panel
```

Key invariants:

- `id` = filename (stable) → re-emits dedupe by frame id.
- `kind:file` carries `url/filename/mime` and **no `content`**; existing kinds
  (markdown/code/html/json/csv) are untouched.
- The wire schema exists in **two mirrors** that must stay identical:
  `internal/stream/aisdk/encoder.go` (live SSE) and
  `internal/eventlog/project.go` (replay/persistence). If the fork has only one
  of these layers, adapt; if it has both, change both.

---

## 4. File inventory

| File | Role | ~Size |
|---|---|---|
| `internal/config/mcp.go` | `MCPConfig`/`MCPServerConfig` schema, `ExpandEnv`, `ExpandedHeaders`, `LoadMCP` (missing file ⇒ empty config, no error) | 90 |
| `internal/mcp/manager.go` | `Manager`: per-server toolsets, `transportFor` validation, `Statuses`, `Toolsets(names)` | 213 |
| `internal/mcp/oauth.go` | `OAuthProvider`: PKCE, pendingStore, RFC 8414 `discover`, RFC 7591 `registerClient`, `Authorize`/`Callback`/`TokenFor` | 407 |
| `internal/mcp/tokenstore.go` | `Token`, `TokenStore` iface, `MemoryTokenStore` + AES-256-GCM `cryptor` from `ENCRYPTION_KEY` | 177 |
| `internal/mcp/roundtripper.go` | `WithUserID` ctx plumbing, `authRoundTripper` (static headers + per-user Bearer), `newAuthHTTPClient` | 96 |
| `internal/mcp/handlers.go` | HTTP: `Connect`, `Callback`, `List`; `redirectURLFor` | 109 |
| `internal/mcp/resilient.go` | `resilientToolset` degrade + **all office logic**: `officeTool`, `firstHTTPURL`, `officeFileArtifact`, MIME map, `OfficeArtifactStatePrefix` | 252 |
| `internal/mcp/{manager,oauth,oauth_flow,roundtripper}_test.go` | no-panic, PKCE/state TTL, store round-trip/encryption/isolation, full authorize→callback→refresh, RoundTripper injection | ~430 total |
| `internal/agent/stream.go` | `officeArtifactFromState` (:44), StateDelta read + `enc.Artifact` (:449); `emit_artifact` special-case (:441) | touch ~40 |
| `internal/stream/aisdk/encoder.go` | `Artifact()` gains url/filename/mime + kind:file (:255–285) | touch ~15 |
| `internal/eventlog/project.go` | `artifactData` mirror (:434–461) | touch ~15 |
| `internal/server/server.go` | route mounts (:64–67) | touch 4 |
| `internal/bootstrap/bootstrap.go` | `LoadMCP` + `mcp.NewManager(cfg, os.Getenv("MCP_OAUTH_REDIRECT_URL"), logger)`; `deps.MCPToolsets = mcpManager.Toolsets` (:248–257) | touch ~12 |
| `internal/tools/registry.go` | `Deps.MCPToolsets func(servers []string) []tool.Toolset` (:52), consumed where agent `mcp_servers` binds toolsets | touch ~5 |
| `config/default/mcp.yaml` | server defs: `office` (remote, :8090/mcp), `gitlab` example (oauth, disabled) | 27 |
| `config/default/agents/swarm-coordinator.md` | frontmatter `mcp_servers: [office]` + prompt guard "NEVER write the file URL" | touch ~10 |
| `services/office-mcp/server.py` | FastMCP server: 3 tools + `/files/{filename}` route + default-template bootstrap | 156 |
| `services/office-mcp/make_template.py`, `requirements.txt`, `templates/` | docxtpl default template builder + deps | small |

---

## 5. Implementation steps

Ordered so each checkpoint compiles and is observable. **Phase C (OAuth) is
optional** — see Known issue (a): the per-user token injection is not wired into
the run path in the source repo, so the OAuth flow is currently inert. Decide up
front whether the fork wants OAuth-capable MCP servers; if not, port Phases A/B
only and leave `oauth: true` servers unsupported (fail `Connect` with 501).

### Phase A — MCP config, static auth, resilience

**A1. Config schema** (`internal/config/mcp.go`). Ensure the fork's
`MCPServerConfig` has: `Type` (`local|remote`, empty ⇒ remote), `URL`,
`Command []string`, `Headers map[string]string`, `OAuth bool`, `Enabled *bool`
(nil ⇒ enabled), plus OAuth fields `AuthorizeURL/TokenURL/RegisterURL/ClientID/
ClientSecret/Scopes`. Add `ExpandEnv` (`${NAME}` → `os.Getenv`, undefined ⇒
empty string) and `ExpandedHeaders()`. `LoadMCP` must return an empty config —
not an error — when `mcp.yaml` is absent.

**A2. Static-header RoundTripper** (`roundtripper.go`). `authRoundTripper`
clones the request (RoundTripper contract), sets each non-empty expanded header,
delegates to `http.DefaultTransport`. `newAuthHTTPClient` wraps it in an
`*http.Client` handed to the streamable transport. This is what makes
`headers: {Authorization: "Bearer ${OFFICE_MCP_KEY}"}` actually reach the
server.

**A3. Manager with no-panic construction** (`manager.go:63-99`). For each
enabled server: `transportFor` validates (`manager.go:189-212` — empty
`command`, empty `url`, unknown type ⇒ error), failures log + set
`StatusFailed` + skip. Remote transport:

```go
client := newAuthHTTPClient(name, sc, m.oauth)
return &mcpsdk.StreamableClientTransport{Endpoint: sc.URL, HTTPClient: client}, nil
```

(`go-sdk mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"` + adk
`mcptoolset`. If the fork uses different MCP client libs, the concept ports: a
custom HTTP client on the streamable transport.)

**A4. Resilient wrapper** (`resilient.go:22-58`). Wrap every toolset:
`Tools()` on error → warn-log + `return nil, nil`. **Port the fixed version**:
also record the failure so status reporting can see it (Known issue (c)/(d)).

**A5. Wire-up.** Bootstrap: load `mcp.yaml`, build manager, expose
`Toolsets(names)` to the agent-construction path keyed off the agent's
`mcp_servers` frontmatter list (`tools/registry.go:52`). Router: mount
`GET /v1/mcp` (status list). (Connect/callback routes come in Phase C.)

**Checkpoint A**: `go build ./... && go vet ./...`. Unit tests: manager
no-panic on `{command: []}`, `{url: ""}`, `{type: "bogus"}` (port
`manager_test.go`); `ExpandEnv`/`ExpandedHeaders` (port `config/mcp_test.go`).
Runtime: start the gateway with the office server **down** — startup clean, a
run by an `mcp_servers: [office]` agent completes with a "degrading to no
tools" warning instead of erroring.

### Phase B — Office server + file-artifact pipeline

**B1. Office MCP server** (`services/office-mcp/server.py`, port verbatim-ish).
FastMCP (`mcp.run(transport="streamable-http")`), env knobs
`OFFICE_OUTPUT_DIR` / `OFFICE_BASE_URL` / `OFFICE_TEMPLATE_DIR` / `OFFICE_PORT`
(default 8090). Three tools returning `f"{BASE_URL}/{name}"`:
`render_report_docx(template, context)` (docxtpl; empty template ⇒
`_ensure_default_template()` builds one via `make_template.py`),
`create_pptx(title, slides)` (python-pptx, `{"heading", "bullets"}` slides),
`create_xlsx(sheet_name, header, rows)` (openpyxl). Critical piece — the
`/files` route on the same app (`server.py:51-61`):

```python
@mcp.custom_route("/files/{filename}", methods=["GET"])
async def serve_file(request: Request) -> Response:
    filename = request.path_params["filename"]
    if "/" in filename or "\\" in filename or filename in ("", ".", ".."):
        return PlainTextResponse("bad filename", status_code=400)
    path = os.path.abspath(os.path.join(OUTPUT_DIR, filename))
    if os.path.commonpath([OUTPUT_DIR, path]) != OUTPUT_DIR or not os.path.isfile(path):
        return PlainTextResponse("not found", status_code=404)
    return FileResponse(path, filename=filename)
```

Keep the traversal guards exactly — this route is unauthenticated by default.

**B2. Artifact wire schema** — extend **both mirrors identically**.
`aisdk/encoder.go Artifact()` (:255): after the existing id/title/kind/content
handling, add optional passthrough of `url`, `filename`, `mime` when non-empty.
`eventlog/project.go artifactData()` (:434): same three fields. `kind:file`
needs no `content`. If the fork's UI has an artifact panel, add a file-card
renderer keyed on `kind === "file"` (download link from `url`, label from
`filename`).

**B3. Office tool decorator** (`resilient.go:97-252`, but see Known issues
(f)/(g)/(h) — extract into `internal/mcp/office.go` + a shared const package,
with tests). Pieces:

- `runnableTool` interface (`resilient.go:105-110`) — structural declaration of
  adk's internal tool method set (`tool.Tool` + `Declaration()` +
  `ProcessRequest` + `Run`) so the decorator can wrap the concrete mcptoolset
  tool without importing adk internals. **Fork check**: verify against the
  fork's adk version; if the method set drifted, the type assertion at
  `resilient.go:47` fails soft (logged, URL not hidden) — good, but confirm.
- `officeTool.ProcessRequest` (`resilient.go:128-155`) — registers **the
  decorator** in `req.Tools[name]` so flow dispatch calls the wrapper's `Run`,
  and appends the declaration to `req.Config.Tools`. This mirrors adk's
  internal `PackTool`; delegating to the embedded inner tool would bypass the
  wrapper entirely. This is the subtlest part of the port — if tool dispatch
  works differently in the fork's adk, find its equivalent "pack tool into LLM
  request" hook.
- `officeTool.Run` (`resilient.go:157-191`) — run inner, `firstHTTPURL(resp, 0)`
  (depth-bounded ≤8 walk over maps/slices — office responses vary in shape:
  `{"output": "<url>"}` and `{"output": {"result": "<url>"}}` both observed),
  build artifact, stash:

```go
actions.StateDelta[OfficeArtifactStatePrefix+ctx.FunctionCallID()] = art
```

  then return the URL-free confirmation map (status/filename/message telling
  the model not to output a URL).
- `officeFileArtifact(url)` (`resilient.go:195-222`) — filename = URL tail
  stripped of `?#`; mime from extension map (pptx/docx/xlsx OOXML types,
  fallback `application/octet-stream`); `id` = filename.
- Wrapping happens inside `resilientToolset.Tools` for `r.name ==
  officeServerName` and `isOfficeToolName(t.Name())` (tolerates adk's
  `office_` namespace prefix).

**B4. Stream layer** (`agent/stream.go`). In the FunctionResponse branch of the
event loop (:434-464), after the `emit_artifact` special-case:

```go
if art := officeArtifactFromState(event.Actions.StateDelta, fr.ID); art != nil {
    enc.Artifact(art)
}
```

with `officeArtifactFromState` (:44-52) doing a nil-safe map lookup on
`officeArtifactStatePrefix + callID`. Note the source repo duplicates the
prefix string here because `agent` → `mcp` would cycle through `handler` —
implement the fixed version instead (Known issue (f)).

**B5. Coordinator binding** (`config/default/agents/swarm-coordinator.md`).
Frontmatter: `mcp_servers: [office]` (this was missing pre-`bab44f3` — config
drift meant the coordinator had no office tools at all). Prompt: a guard
paragraph — documents surface automatically as artifact cards; NEVER write the
file URL or a "download here" link. Belt-and-braces on top of the decorator.

**Checkpoint B**: see [Verification](#7-verification).

### Phase C (optional) — backend-held OAuth

Skip entirely if the fork has no OAuth MCP servers planned; nothing in Phases
A/B depends on it (the provider is constructed but dormant for non-oauth
servers).

**C1. Token store** (`tokenstore.go`). `Token` with `Expired()` (30s skew, zero
Expiry ⇒ non-expiring), `TokenStore` interface, `MemoryTokenStore` sealing the
JSON blob with AES-256-GCM when `ENCRYPTION_KEY` is set (key = SHA-256 of the
env value, so any length works; `tokenstore.go:61-75`). Interface is
Valkey/Redis-ready if the fork wants persistence.

**C2. Provider** (`oauth.go`). Port `pkce`/`newPKCE` (S256), `pendingStore`
(5-min TTL, single-use `take`, opportunistic GC on `put`), `resolveEndpoints`
(config-first, RFC 8414 fallback at
`<origin>/.well-known/oauth-authorization-server`), `registerClient` (RFC 7591,
`token_endpoint_auth_method: "none"` public client), `Authorize`, `Callback`,
`TokenFor` (+refresh, keeping the old refresh token when the provider omits a
new one — `oauth.go:372-375`). **Apply Known issues (b) and (e) while porting.**

**C3. Routes + handlers** (`handlers.go`, `server/server.go:64-67`):

```
GET  /v1/mcp                      → servers + per-user status
GET  /v1/mcp/oauth/callback       → state/exchange/store, tiny HTML "you can close this"
POST /v1/mcp/{server}/connect     → {"authorize_url": ...}
```

`userID` from the fork's auth middleware (source: `handler.UserID(r)`,
`internal/handler/identity.go:15`).

**C4. RoundTripper OAuth branch** (`roundtripper.go:62-75`): when `sc.OAuth`,
read userID from the request context, `TokenFor`, set
`Authorization: <type> <token>`; on error leave unset (server 401s, status
shows needs_auth). **C5 — the wiring the source repo never did**: call
`mcp.WithUserID(ctx, userID)` on the context the run path passes into the ADK
runner, so tool-call HTTP requests carry it. See Known issue (a).

**Checkpoint C**: port the test files — `oauth_test.go` (PKCE shape, state
TTL/single-use), `oauth_flow_test.go` (httptest fake provider:
authorize→callback→refresh end-to-end), `roundtripper_test.go` (static headers
+ per-user Bearer from context), token store round-trip/encryption/isolation
tests. All green + a live `connect` against a real provider if available.

---

## 6. Known issues — fix during port

These are defects in the source repo as merged. **Implement the fixed versions
in the fork**; do not faithfully reproduce them.

**(a) OAuth is inert: `WithUserID` is never called in the run path.**
`roundtripper.go:20-25` documents the deferred binding; no caller of
`mcp.WithUserID` exists outside tests. Consequence: `userIDFromContext` always
returns `""`, the Bearer branch never fires, and every `oauth: true` server
401s on tool calls even after a successful connect. Fork decision, up front:
either (i) wire it — find where the fork constructs the context passed to the
ADK runner (the streaming run entrypoint and any background/resume paths) and
wrap it with `mcp.WithUserID(ctx, userID)`; the userID is already in scope in
`StreamAgentRunFormat` (`stream.go:90`) — or (ii) consciously skip Phase C and
make `Connect` return a clear "OAuth not supported" error. Do not port the
half-wired middle state.

**(b) Dynamically-registered `client_id` is lost after the callback.**
`registerClient`'s result lives only in the transient `pendingAuth`
(`oauth.go:258-265`). `TokenFor`'s refresh path re-reads
`config.ExpandEnv(sc.ClientID)` (`oauth.go:361`) — empty for
dynamically-registered servers — so the first post-expiry refresh sends no
`client_id` and most providers reject it. Fix: persist the client_id with the
token (add `ClientID string` to `Token`, set it in `Callback`, prefer it over
config in the refresh path), or add a `clientIDs map[server]string` on the
provider persisted alongside the store.

**(c) Server status lies: `StatusConnected` at construction, forever.**
`manager.go:95` sets `StatusConnected` when the toolset is *built* — no
connection has happened (adk connects lazily on first `Tools()`), and
`resilientToolset` swallows every later failure, so `baseline` never
transitions to failed. `GET /v1/mcp` reports a dead server as connected
indefinitely. Fix: rename the baseline semantics (e.g. `configured`) or derive
status from a live probe — have `resilientToolset` report last-listing
success/failure back to the manager (timestamped `lastErr` per server), and
have `Statuses` consult it.

**(d) 401 and outage are indistinguishable — both become silent `nil, nil`.**
`resilient.go:31-35` flattens every listing error. A 401 from an OAuth server
means "this user needs to (re)connect" and should surface as `needs_auth` in
`/v1/mcp` and ideally as a UI hint on the run; an outage means "degrade
quietly". Fix: inspect the error (the streamable transport surfaces HTTP
status; match on 401/403) and record `needs_auth` vs `unreachable` in the
per-server status from (c).

**(e) `redirect_uri` built from spoofable proxy headers.**
`redirectURLFor` (`handlers.go:20-33`) trusts `X-Forwarded-Proto`/`X-Forwarded-Host`
from any client. An attacker-influenced redirect_uri is registered with the
provider (RFC 7591 body, `oauth.go:195`) and sent on authorize. Fix: prefer the
configured `MCP_OAUTH_REDIRECT_URL` (already read in `bootstrap.go:256` and
passed to `NewOAuthProvider`) whenever set, and only fall back to
request-derived values behind a trusted-proxy allowlist. In the fork: make the
env var the primary path and log a warning when falling back.

**(f) `"office:artifact:"` prefix duplicated with MUST-match comments.**
Defined in `internal/mcp/resilient.go:68` (`OfficeArtifactStatePrefix`) and
re-declared as a literal in `internal/agent/stream.go:37` because importing
`mcp` from `agent` would cycle via `handler`. Fix: move the const (plus, ideally,
the artifact map keys) into a tiny leaf package with no deps — e.g.
`internal/mcpkeys` or an existing shared-consts package in the fork — and
import it from both sides.

**(g) ~150 lines of office-specific logic living in `resilient.go`.**
`officeServerName`, tool-name matching, MIME map, `officeTool`,
`officeFileArtifact`, `firstHTTPURL` are all office concerns bolted onto the
generic resilience wrapper (`resilient.go:60-252`). Fix: extract
`internal/mcp/office.go`; `resilientToolset.Tools` keeps only a one-line hook
(`tools = wrapOfficeTools(r.name, tools, r.logger)`).

**(h) Zero tests on the office helpers.** `firstHTTPURL` (depth bound, both
observed response shapes, non-URL strings, slices), `officeFileArtifact`
(query/fragment stripping, extension→MIME incl. unknown ⇒ octet-stream, empty
filename ⇒ id=url), `isOfficeToolName` (bare, `office_`-prefixed, unrelated
names) are pure functions with no coverage. Add table tests in the fork —
they're ten minutes of work and this is exactly the code that silently breaks
when a server changes its response shape.

---

## 7. Fork-adaptation notes

- **`mcp.yaml` schema/location**: source loads `<configDir>/mcp.yaml`
  (`bootstrap.go:248`); default at `config/default/mcp.yaml`. If the fork's
  config layering differs (env overlays, single config file), fold the
  `servers:` map in wherever equivalent — the loader contract to preserve is
  *missing file ⇒ empty config ⇒ gateway runs normally*.
- **Env vars**: `ENCRYPTION_KEY` (token at-rest encryption — unset means
  plaintext-in-memory dev mode; set it in any real deployment),
  `MCP_OAUTH_REDIRECT_URL` (per (e), make this the primary redirect source),
  `OFFICE_OUTPUT_DIR`, `OFFICE_BASE_URL`, `OFFICE_TEMPLATE_DIR`, `OFFICE_PORT`.
  **`OFFICE_BASE_URL` must be a URL the *browser* can reach** — the artifact
  card's download link is this URL verbatim. `localhost:8090` only works for
  same-host dev; behind Docker/reverse proxy set it to the externally routable
  files URL.
- **Office server deps**: Python ≥3.10 venv;
  `pip install -r services/office-mcp/requirements.txt`
  (`mcp>=1.2.0, docxtpl>=0.16.0, python-docx>=1.1.0, python-pptx>=1.0.0,
  openpyxl>=3.1.0`). Ship `make_template.py` + `templates/` alongside
  `server.py` — the default docx template is generated on first use.
- **adk/go-sdk versions**: `runnableTool` + `ProcessRequest` packing
  (`resilient.go:97-155`) is coupled to adk's internal tool dispatch, and the
  streamable transport must accept a custom `HTTPClient` and must issue
  requests with the tool-call context (`http.NewRequestWithContext`) for
  `WithUserID` to work. Verify both against the fork's pinned versions before
  porting the decorator; if dispatch differs, the fallback design is
  string-scrubbing the FunctionResponse (worse — the model sees a mutated
  result) or a model-visible instruction only (worst).
- **Agent frontmatter**: `mcp_servers` must be parsed by the fork's roster
  loader (`roster/load_markdown.go:36`, `config/agents.go:50` in source) and
  reach toolset construction. If the fork renamed the field, keep its name —
  just bind the office server to whichever coordinator-equivalent agent should
  produce documents.
- **UI**: the fork's artifact panel needs a `kind:"file"` branch. Frames arrive
  as `data-artifact` parts (aisdk) with `{url, filename, mime}` and empty
  `content`; persisted messages reconstruct the same via the eventlog mirror.

---

## 8. Verification

**Office pipeline smoke (Checkpoint B):**

```bash
# 1. Start office server
cd services/office-mcp && python server.py &          # :8090

# 2. Gateway up with office enabled in mcp.yaml; then drive a run
#    through an mcp_servers:[office] agent:
#    "Create a PowerPoint titled Q3 Review with 3 slides about ..."

# 3. The SSE stream must contain a data-artifact frame shaped:
#    {"type":"data-artifact","id":"deck-<hex>.pptx","data":{
#      "id":"deck-<hex>.pptx","kind":"file","title":"deck-<hex>.pptx",
#      "content":"","url":"http://.../files/deck-<hex>.pptx",
#      "filename":"deck-<hex>.pptx",
#      "mime":"application/vnd.openxmlformats-officedocument.presentationml.presentation"}}

# 4. URL serves a real OOXML file — PK zip magic bytes:
curl -s http://localhost:8090/files/deck-<hex>.pptx | head -c 4 | xxd
# expect: 504b 0304  ("PK..")

# 5. Traversal guard:
curl -s -o /dev/null -w '%{http_code}\n' "http://localhost:8090/files/..%2fserver.py"   # 400/404
```

Then assert: (i) the **final assistant text contains no `/files/` URL** (the
decorator worked — check both an Anthropic and a non-Anthropic model if the
fork is multi-provider; source verified on Claude Sonnet 4.5 and GPT-4.1);
(ii) `GET /threads/{id}/messages` replays the artifact part with the same
shape (eventlog mirror works); (iii) with the office server **stopped**, the
same prompt yields a completed run plus the "degrading to no tools" log line.

**Resilience/config (Checkpoint A):** manager tests prove no panic on empty
command / empty url / unknown type; `GET /v1/mcp` lists servers with statuses
(post-fix (c): a stopped office server must NOT show `connected`).

**OAuth (Checkpoint C, if ported):** unit suite green (PKCE S256 vector, state
single-use + TTL expiry, store encryption round-trip + cross-user isolation,
httptest full flow incl. refresh); then live: `POST /v1/mcp/gitlab/connect` →
browser consents → callback 200 → `GET /v1/mcp` shows `connected` for that
user and `needs_auth` for another; an MCP tool call from a run carries
`Authorization: Bearer ...` (observable via a logging proxy or the fake
provider) — this last check is the proof that fix (a) actually landed.
