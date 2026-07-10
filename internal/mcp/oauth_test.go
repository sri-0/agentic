package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestNewPKCE_S256(t *testing.T) {
	pk, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	if pk.Verifier == "" || pk.Challenge == "" {
		t.Fatal("empty verifier/challenge")
	}
	// Challenge must be base64url(SHA256(verifier)), no padding.
	sum := sha256.Sum256([]byte(pk.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pk.Challenge != want {
		t.Fatalf("challenge mismatch: got %q want %q", pk.Challenge, want)
	}
	// Two PKCE pairs must differ.
	pk2, _ := newPKCE()
	if pk2.Verifier == pk.Verifier {
		t.Fatal("verifier not random")
	}
}

func TestPendingStore_StateSingleUseAndTTL(t *testing.T) {
	ps := newPendingStore()
	a := pendingAuth{userID: "u1", server: "gitlab", created: time.Now()}
	ps.put("state123", a)

	// First take succeeds.
	got, ok := ps.take("state123")
	if !ok || got.userID != "u1" {
		t.Fatalf("take failed: ok=%v got=%+v", ok, got)
	}
	// Second take fails (single use / CSRF replay protection).
	if _, ok := ps.take("state123"); ok {
		t.Fatal("state should be single-use")
	}
	// Unknown state fails.
	if _, ok := ps.take("nope"); ok {
		t.Fatal("unknown state should fail")
	}

	// Expired state fails.
	ps.put("old", pendingAuth{userID: "u2", created: time.Now().Add(-2 * stateTTL)})
	if _, ok := ps.take("old"); ok {
		t.Fatal("expired state should fail")
	}
}

func TestTokenExpired(t *testing.T) {
	cases := []struct {
		name string
		tok  *Token
		want bool
	}{
		{"nil", nil, true},
		{"no-expiry", &Token{AccessToken: "x"}, false},
		{"future", &Token{Expiry: time.Now().Add(time.Hour)}, false},
		{"past", &Token{Expiry: time.Now().Add(-time.Hour)}, true},
		{"within-skew", &Token{Expiry: time.Now().Add(10 * time.Second)}, true},
	}
	for _, c := range cases {
		if got := c.tok.Expired(); got != c.want {
			t.Errorf("%s: Expired()=%v want %v", c.name, got, c.want)
		}
	}
}

func TestMemoryTokenStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	// Force encryption on by setting a key for this store.
	t.Setenv("ENCRYPTION_KEY", "test-key-material-1234567890")
	s := NewMemoryTokenStore()
	if s.crypto == nil {
		t.Fatal("expected at-rest encryption to be enabled with ENCRYPTION_KEY set")
	}

	if _, err := s.Get(ctx, "u1", "gitlab"); err != ErrNoToken {
		t.Fatalf("expected ErrNoToken, got %v", err)
	}

	tok := &Token{AccessToken: "abc", RefreshToken: "ref", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
	if err := s.Put(ctx, "u1", "gitlab", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Blob on disk must be ciphertext (not contain the plaintext token).
	blob := s.blobs[tokenKey("u1", "gitlab")]
	if blob == "" {
		t.Fatal("no blob stored")
	}
	if containsPlain(blob, "abc") || containsPlain(blob, "ref") {
		t.Fatal("token stored in plaintext")
	}

	got, err := s.Get(ctx, "u1", "gitlab")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessToken != "abc" || got.RefreshToken != "ref" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Isolation: different user has no token.
	if _, err := s.Get(ctx, "u2", "gitlab"); err != ErrNoToken {
		t.Fatalf("expected per-user isolation, got %v", err)
	}

	if err := s.Delete(ctx, "u1", "gitlab"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "u1", "gitlab"); err != ErrNoToken {
		t.Fatal("expected token deleted")
	}
}

func TestMemoryTokenStore_NoKeyStoresPlain(t *testing.T) {
	// With no ENCRYPTION_KEY the store still works (dev fallback), just unencrypted.
	t.Setenv("ENCRYPTION_KEY", "")
	s := NewMemoryTokenStore()
	if s.crypto != nil {
		t.Fatal("expected no crypto without ENCRYPTION_KEY")
	}
	ctx := context.Background()
	if err := s.Put(ctx, "u", "srv", &Token{AccessToken: "z"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "u", "srv")
	if err != nil || got.AccessToken != "z" {
		t.Fatalf("plain round-trip failed: %v %+v", err, got)
	}
}

func containsPlain(haystack, needle string) bool {
	// base64 of ciphertext is very unlikely to contain the raw ASCII token,
	// but check the decoded form too by simple substring on the raw blob.
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
