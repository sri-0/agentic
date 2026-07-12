package handler

import "net/http"

// DefaultUserID is the identity used when no user header is present. All
// per-user state (sessions, threads, memories, MCP tokens) is keyed by the
// resolved user id, so this is the single-tenant fallback until real auth lands.
const DefaultUserID = "anonymous"

// UserID is the single identity-resolution seam for the whole gateway. Today it
// reads the X-User-ID header and falls back to DefaultUserID; when SSO/JWT is
// added, this is the one place to swap in token extraction. Keep all handlers
// and the run coordinator routing through this function rather than reading the
// header directly.
func UserID(r *http.Request) string {
	if uid := r.Header.Get("X-User-ID"); uid != "" {
		return uid
	}
	return DefaultUserID
}
