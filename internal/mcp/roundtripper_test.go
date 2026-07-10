package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentic/internal/config"
)

// TestAuthRoundTripper_StaticHeaders proves H3a: static (API-key) headers are
// injected on every request after ${ENV} expansion.
func TestAuthRoundTripper_StaticHeaders(t *testing.T) {
	t.Setenv("OFFICE_MCP_KEY", "s3cr3t")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sc := config.MCPServerConfig{
		Type:    "remote",
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${OFFICE_MCP_KEY}"},
	}
	client := newAuthHTTPClient("office", sc, nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("static header not injected/expanded: got %q", gotAuth)
	}
}

// TestAuthRoundTripper_OAuthPerUser proves the per-user bearer injection: the
// RoundTripper reads the user id from the request context (WithUserID) and
// injects that user's stored token.
func TestAuthRoundTripper_OAuthPerUser(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "k")
	store := NewMemoryTokenStore()
	ctx := context.Background()
	_ = store.Put(ctx, "alice", "gitlab", &Token{AccessToken: "alice-tok", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
	_ = store.Put(ctx, "bob", "gitlab", &Token{AccessToken: "bob-tok", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
	prov := NewOAuthProvider(store, "http://cb")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sc := config.MCPServerConfig{Type: "remote", URL: srv.URL, OAuth: true}
	client := newAuthHTTPClient("gitlab", sc, prov)

	// alice
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req = req.WithContext(WithUserID(req.Context(), "alice"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer alice-tok" {
		t.Fatalf("alice token not injected: got %q", gotAuth)
	}

	// bob (same shared client, different user via context)
	req2, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req2 = req2.WithContext(WithUserID(req2.Context(), "bob"))
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if gotAuth != "Bearer bob-tok" {
		t.Fatalf("bob token not injected: got %q", gotAuth)
	}

	// no user in context → no Authorization (server returns 401 → needs_auth)
	req3, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if gotAuth != "" {
		t.Fatalf("expected no auth without user context, got %q", gotAuth)
	}
}
