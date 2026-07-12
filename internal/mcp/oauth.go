package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentic/internal/config"
)

// PKCE (RFC 7636) + CSRF state (RFC 6749 §10.12) parameters for one in-flight
// authorization. Held in the pendingStore keyed by `state` until the callback
// completes the exchange; expires after stateTTL.
type pkce struct {
	Verifier  string
	Challenge string
}

// newPKCE generates a fresh S256 PKCE pair.
func newPKCE() (pkce, error) {
	v, err := randToken(32)
	if err != nil {
		return pkce{}, err
	}
	sum := sha256.Sum256([]byte(v))
	return pkce{Verifier: v, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// randToken returns a URL-safe random token with n bytes of entropy.
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// stateTTL bounds how long a `state` (and its PKCE verifier) is valid — the
// 5-minute callback timeout from the Phase 04 plan.
const stateTTL = 5 * time.Minute

// pendingAuth is one buffered authorization awaiting its callback.
type pendingAuth struct {
	userID   string
	server   string
	pkce     pkce
	clientID string // resolved (may come from dynamic registration)
	redirect string // redirect_uri used for this flow (must match on exchange)
	created  time.Time
}

// pendingStore holds in-flight authorizations keyed by CSRF state, with TTL
// expiry. A "pending" provider buffers PKCE/clientID in memory and only the
// callback (with a matching state) can complete the flow.
type pendingStore struct {
	mu sync.Mutex
	m  map[string]pendingAuth
}

func newPendingStore() *pendingStore { return &pendingStore{m: map[string]pendingAuth{}} }

func (p *pendingStore) put(state string, a pendingAuth) {
	p.mu.Lock()
	p.m[state] = a
	// opportunistic GC of expired entries.
	for k, v := range p.m {
		if time.Since(v.created) > stateTTL {
			delete(p.m, k)
		}
	}
	p.mu.Unlock()
}

// take validates and consumes a state. Returns (auth, true) only if the state
// exists and is within TTL; the entry is always removed (single use).
func (p *pendingStore) take(state string) (pendingAuth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.m[state]
	if !ok {
		return pendingAuth{}, false
	}
	delete(p.m, state)
	if time.Since(a.created) > stateTTL {
		return pendingAuth{}, false
	}
	return a, true
}

// OAuthProvider drives the backend-held OAuth flow for MCP servers: authorize
// URL construction (PKCE + optional RFC 7591 dynamic registration), callback
// code→token exchange, and refresh. Tokens land in the TokenStore keyed by
// (userID, server).
type OAuthProvider struct {
	store    TokenStore
	pending  *pendingStore
	http     *http.Client
	redirect string // absolute callback URL (GET /v1/mcp/oauth/callback)
}

// NewOAuthProvider builds a provider. redirectURL is the gateway's public
// callback URL that the provider will redirect back to.
func NewOAuthProvider(store TokenStore, redirectURL string) *OAuthProvider {
	return &OAuthProvider{
		store:    store,
		pending:  newPendingStore(),
		http:     &http.Client{Timeout: 30 * time.Second},
		redirect: redirectURL,
	}
}

// oauthMeta is the subset of RFC 8414 authorization-server metadata we consume.
type oauthMeta struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// resolveEndpoints returns (authorize, token, register) URLs for a server,
// preferring explicit config and falling back to RFC 8414 discovery from the
// MCP server's origin.
func (p *OAuthProvider) resolveEndpoints(ctx context.Context, sc config.MCPServerConfig) (authorize, token, register string, err error) {
	authorize = config.ExpandEnv(sc.AuthorizeURL)
	token = config.ExpandEnv(sc.TokenURL)
	register = config.ExpandEnv(sc.RegisterURL)
	if authorize != "" && token != "" {
		return authorize, token, register, nil
	}
	meta, derr := p.discover(ctx, sc.URL)
	if derr != nil {
		if authorize == "" || token == "" {
			return "", "", "", fmt.Errorf("mcp oauth: no endpoints configured and discovery failed: %w", derr)
		}
		return authorize, token, register, nil
	}
	if authorize == "" {
		authorize = meta.AuthorizationEndpoint
	}
	if token == "" {
		token = meta.TokenEndpoint
	}
	if register == "" {
		register = meta.RegistrationEndpoint
	}
	if authorize == "" || token == "" {
		return "", "", "", fmt.Errorf("mcp oauth: server did not advertise authorize/token endpoints")
	}
	return authorize, token, register, nil
}

// discover fetches RFC 8414 metadata from <origin>/.well-known/oauth-authorization-server.
func (p *OAuthProvider) discover(ctx context.Context, serverURL string) (*oauthMeta, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	metaURL := u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery status %d", resp.StatusCode)
	}
	var m oauthMeta
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// registerClient performs RFC 7591 dynamic client registration, returning a
// client_id. Best-effort: a clear error surfaces if the server does not support
// it.
func (p *OAuthProvider) registerClient(ctx context.Context, registerURL, redirectURL string, scopes []string) (string, error) {
	if registerURL == "" {
		return "", fmt.Errorf("mcp oauth: no client_id configured and server has no registration endpoint (RFC 7591 unsupported)")
	}
	body := map[string]any{
		"client_name":                "agentic-gateway",
		"redirect_uris":              []string{redirectURL},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none", // public client (PKCE)
	}
	if len(scopes) > 0 {
		body["scope"] = strings.Join(scopes, " ")
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registerURL, strings.NewReader(string(raw)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("mcp oauth: dynamic registration failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("mcp oauth: registration response had no client_id")
	}
	return out.ClientID, nil
}

// Authorize builds the provider authorize URL for (userID, server) and buffers
// the PKCE verifier + state until the callback. Uses RFC 7591 dynamic client
// registration when no client_id is configured. redirectURL is the gateway
// callback the provider should return to; when empty the provider default is
// used.
func (p *OAuthProvider) Authorize(ctx context.Context, userID, server string, sc config.MCPServerConfig, redirectURL string) (string, error) {
	if redirectURL == "" {
		redirectURL = p.redirect
	}
	authorize, _, register, err := p.resolveEndpoints(ctx, sc)
	if err != nil {
		return "", err
	}
	clientID := config.ExpandEnv(sc.ClientID)
	if clientID == "" {
		clientID, err = p.registerClient(ctx, register, redirectURL, sc.Scopes)
		if err != nil {
			return "", err
		}
	}
	pk, err := newPKCE()
	if err != nil {
		return "", err
	}
	state, err := randToken(24)
	if err != nil {
		return "", err
	}
	p.pending.put(state, pendingAuth{
		userID:   userID,
		server:   server,
		pkce:     pk,
		clientID: clientID,
		redirect: redirectURL,
		created:  time.Now(),
	})

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURL)
	q.Set("state", state)
	q.Set("code_challenge", pk.Challenge)
	q.Set("code_challenge_method", "S256")
	if len(sc.Scopes) > 0 {
		q.Set("scope", strings.Join(sc.Scopes, " "))
	}
	sep := "?"
	if strings.Contains(authorize, "?") {
		sep = "&"
	}
	return authorize + sep + q.Encode(), nil
}

// tokenResponse is the RFC 6749 token endpoint response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (tr tokenResponse) toToken() *Token {
	t := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		t.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return t
}

// Callback validates state (CSRF + 5-min TTL), exchanges code→token, and stores
// it keyed by (userID, server). Returns the resolved (userID, server) so the
// handler can report which connection completed.
func (p *OAuthProvider) Callback(ctx context.Context, state, code string, servers map[string]config.MCPServerConfig) (userID, server string, err error) {
	auth, ok := p.pending.take(state)
	if !ok {
		return "", "", fmt.Errorf("mcp oauth: invalid or expired state")
	}
	sc, ok := servers[auth.server]
	if !ok {
		return "", "", fmt.Errorf("mcp oauth: unknown server %q", auth.server)
	}
	_, tokenURL, _, err := p.resolveEndpoints(ctx, sc)
	if err != nil {
		return "", "", err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", auth.redirect)
	form.Set("client_id", auth.clientID)
	form.Set("code_verifier", auth.pkce.Verifier)
	if secret := config.ExpandEnv(sc.ClientSecret); secret != "" {
		form.Set("client_secret", secret)
	}
	tok, err := p.postToken(ctx, tokenURL, form)
	if err != nil {
		return "", "", err
	}
	if err := p.store.Put(ctx, auth.userID, auth.server, tok); err != nil {
		return "", "", err
	}
	return auth.userID, auth.server, nil
}

// TokenFor returns a valid access token for (userID, server), refreshing it if
// expired and a refresh token is available. Returns ErrNoToken if none stored.
func (p *OAuthProvider) TokenFor(ctx context.Context, userID, server string, sc config.MCPServerConfig) (*Token, error) {
	tok, err := p.store.Get(ctx, userID, server)
	if err != nil {
		return nil, err
	}
	if !tok.Expired() {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("mcp oauth: token expired and no refresh token: %w", ErrNoToken)
	}
	_, tokenURL, _, err := p.resolveEndpoints(ctx, sc)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", tok.RefreshToken)
	clientID := config.ExpandEnv(sc.ClientID)
	if clientID != "" {
		form.Set("client_id", clientID)
	}
	if secret := config.ExpandEnv(sc.ClientSecret); secret != "" {
		form.Set("client_secret", secret)
	}
	refreshed, err := p.postToken(ctx, tokenURL, form)
	if err != nil {
		return nil, err
	}
	// Providers may omit a new refresh token on refresh; keep the old one.
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tok.RefreshToken
	}
	if err := p.store.Put(ctx, userID, server, refreshed); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// postToken performs a token-endpoint POST and parses the response.
func (p *OAuthProvider) postToken(ctx context.Context, tokenURL string, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp oauth: token endpoint status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("mcp oauth: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("mcp oauth: token response had no access_token")
	}
	return tr.toToken(), nil
}
