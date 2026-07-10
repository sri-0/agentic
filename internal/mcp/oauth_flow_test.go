package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agentic/internal/config"
)

// TestOAuthFlow_AuthorizeCallbackRefresh drives the full backend-held OAuth flow
// against a fake provider: authorize URL (PKCE) → code exchange at callback →
// stored token → refresh when expired. This is the logic verification the plan
// asks for since no live GitLab is available.
func TestOAuthFlow_AuthorizeCallbackRefresh(t *testing.T) {
	var lastVerifier, lastGrant string
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			lastGrant = r.Form.Get("grant_type")
			lastVerifier = r.Form.Get("code_verifier")
			w.Header().Set("Content-Type", "application/json")
			if r.Form.Get("grant_type") == "refresh_token" {
				_, _ = w.Write([]byte(`{"access_token":"new-access","token_type":"Bearer","expires_in":3600}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"first-access","refresh_token":"r1","token_type":"Bearer","expires_in":3600}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeProvider.Close()

	sc := config.MCPServerConfig{
		Type:         "remote",
		URL:          "http://mcp.example/mcp",
		OAuth:        true,
		AuthorizeURL: fakeProvider.URL + "/authorize",
		TokenURL:     fakeProvider.URL + "/token",
		ClientID:     "client-abc",
		Scopes:       []string{"read_api"},
	}
	store := NewMemoryTokenStore()
	prov := NewOAuthProvider(store, "http://gw/v1/mcp/oauth/callback")
	ctx := context.Background()

	// 1. Authorize → parse the URL, extract state + verify PKCE S256 params.
	authURL, err := prov.Authorize(ctx, "alice", "gitlab", sc, "http://gw/v1/mcp/oauth/callback")
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	u, _ := url.Parse(authURL)
	if !strings.HasPrefix(authURL, fakeProvider.URL+"/authorize") {
		t.Fatalf("wrong authorize base: %s", authURL)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("missing PKCE params: %v", q)
	}
	if q.Get("client_id") != "client-abc" || q.Get("scope") != "read_api" {
		t.Fatalf("wrong client/scope: %v", q)
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("no state")
	}

	// 2. Callback with the state + a code → token exchange + storage.
	servers := map[string]config.MCPServerConfig{"gitlab": sc}
	uid, srv, err := prov.Callback(ctx, state, "auth-code-xyz", servers)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if uid != "alice" || srv != "gitlab" {
		t.Fatalf("callback resolved wrong (uid=%s srv=%s)", uid, srv)
	}
	if lastGrant != "authorization_code" || lastVerifier == "" {
		t.Fatalf("exchange did not send PKCE verifier (grant=%s verifier=%q)", lastGrant, lastVerifier)
	}

	tok, err := store.Get(ctx, "alice", "gitlab")
	if err != nil || tok.AccessToken != "first-access" {
		t.Fatalf("token not stored: %v %+v", err, tok)
	}

	// 3. Force expiry → TokenFor refreshes.
	tok.Expiry = tok.Expiry.AddDate(-1, 0, 0)
	_ = store.Put(ctx, "alice", "gitlab", tok)
	refreshed, err := prov.TokenFor(ctx, "alice", "gitlab", sc)
	if err != nil {
		t.Fatalf("TokenFor refresh: %v", err)
	}
	if refreshed.AccessToken != "new-access" {
		t.Fatalf("expected refreshed token, got %q", refreshed.AccessToken)
	}
	if lastGrant != "refresh_token" {
		t.Fatalf("expected refresh grant, got %q", lastGrant)
	}
	// Refresh token preserved when provider omits a new one.
	if refreshed.RefreshToken != "r1" {
		t.Fatalf("refresh token not preserved: %q", refreshed.RefreshToken)
	}

	// 4. Replayed state is rejected (CSRF single-use).
	if _, _, err := prov.Callback(ctx, state, "auth-code-xyz", servers); err == nil {
		t.Fatal("expected replayed state to be rejected")
	}
}
