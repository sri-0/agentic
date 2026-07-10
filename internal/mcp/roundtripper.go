package mcp

import (
	"context"
	"net/http"

	"agentic/internal/config"
)

// userIDKey types the context value carrying the calling user's ID down into the
// MCP transport, so the RoundTripper can look up that user's OAuth token.
type userIDKey struct{}

// WithUserID returns ctx annotated with userID. The MCP tool-call context must
// carry this for per-user OAuth token injection to work: the go-sdk streamable
// transport issues its HTTP POST with the tool-call context
// (http.NewRequestWithContext in streamable.go), so a value set here reaches the
// RoundTripper below.
//
// WIRING NOTE (deferred final binding): the run coordinator / handler must call
// WithUserID on the context that ADK threads into the agent run, so that
// mcptoolset's tool.Context (a context.Context) carries it. Until that call is
// added in the run path, the RoundTripper falls back to the empty user and the
// static-header path (H3a) still works; OAuth-per-user is wired end-to-end
// except this one binding. See report.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// userIDFromContext extracts the user id previously set by WithUserID.
func userIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userIDKey{}).(string); ok {
		return v
	}
	return ""
}

// authRoundTripper injects per-request authentication into outgoing MCP HTTP
// requests: static headers (H3a) always, plus a per-user OAuth bearer token when
// the server is configured with oauth: true. It reads the user id from the
// request context (WithUserID), so a single shared transport serves every user
// with the correct credential.
type authRoundTripper struct {
	base    http.RoundTripper
	server  string
	sc      config.MCPServerConfig
	headers map[string]string // pre-expanded static headers
	oauth   *OAuthProvider    // nil when the server is not oauth
}

func (t *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating headers (RoundTripper contract: must not modify the
	// caller's request).
	r := req.Clone(req.Context())

	for k, v := range t.headers {
		if v != "" {
			r.Header.Set(k, v)
		}
	}

	if t.sc.OAuth && t.oauth != nil {
		userID := userIDFromContext(req.Context())
		if userID != "" {
			if tok, err := t.oauth.TokenFor(req.Context(), userID, t.server, t.sc); err == nil && tok.AccessToken != "" {
				typ := tok.TokenType
				if typ == "" {
					typ = "Bearer"
				}
				r.Header.Set("Authorization", typ+" "+tok.AccessToken)
			}
			// On error (no/expired token) we leave Authorization unset; the
			// server returns 401 and status surfaces as needs_auth.
		}
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// newAuthHTTPClient builds an *http.Client whose Transport injects the server's
// static headers and (when oauth) the per-user bearer token.
func newAuthHTTPClient(server string, sc config.MCPServerConfig, oauth *OAuthProvider) *http.Client {
	return &http.Client{
		Transport: &authRoundTripper{
			base:    http.DefaultTransport,
			server:  server,
			sc:      sc,
			headers: sc.ExpandedHeaders(),
			oauth:   oauth,
		},
	}
}
