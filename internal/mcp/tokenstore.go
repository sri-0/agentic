package mcp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Token is a backend-held OAuth credential for one (userID, server) pair. It is
// stored encrypted at rest and never leaves the process in plaintext except when
// injected into an outgoing MCP request Authorization header.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// Expired reports whether the access token has expired (with a 30s skew). A
// zero Expiry means "no known expiry" → treated as non-expiring.
func (t *Token) Expired() bool {
	if t == nil {
		return true
	}
	if t.Expiry.IsZero() {
		return false
	}
	return time.Now().Add(30 * time.Second).After(t.Expiry)
}

// ErrNoToken is returned by TokenStore.Get when no token exists for the key.
var ErrNoToken = errors.New("mcp: no token stored")

// TokenStore persists OAuth tokens keyed by (userID, server), encrypted at rest.
// The in-memory implementation is the default; a Valkey-backed one can wrap the
// same crypto via the same interface.
type TokenStore interface {
	Get(ctx context.Context, userID, server string) (*Token, error)
	Put(ctx context.Context, userID, server string, tok *Token) error
	Delete(ctx context.Context, userID, server string) error
}

// cryptor performs AES-256-GCM encryption using a key derived from ENCRYPTION_KEY.
type cryptor struct {
	gcm cipher.AEAD
}

// newCryptor builds a cryptor from the raw key material. The key is hashed to a
// stable 32-byte AES-256 key so any-length ENCRYPTION_KEY works.
func newCryptor(key []byte) (*cryptor, error) {
	if len(key) == 0 {
		return nil, errors.New("mcp: empty encryption key")
	}
	sum := sha256.Sum256(key)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("mcp: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("mcp: gcm: %w", err)
	}
	return &cryptor{gcm: gcm}, nil
}

// encryptionKeyFromEnv reads ENCRYPTION_KEY. Empty is allowed (the memory store
// falls back to storing tokens without at-rest encryption in that dev case, but
// callers should set it in any real deployment).
func encryptionKeyFromEnv() []byte {
	return []byte(os.Getenv("ENCRYPTION_KEY"))
}

func (c *cryptor) seal(plaintext []byte) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := c.gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (c *cryptor) open(s string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	ns := c.gcm.NonceSize()
	if len(data) < ns {
		return nil, errors.New("mcp: ciphertext too short")
	}
	nonce, ct := data[:ns], data[ns:]
	return c.gcm.Open(nil, nonce, ct, nil)
}

// MemoryTokenStore is an in-process TokenStore. When ENCRYPTION_KEY is set the
// serialized token blob is AES-GCM encrypted before it lands in the map, so a
// heap/core dump does not leak plaintext refresh tokens.
type MemoryTokenStore struct {
	mu     sync.RWMutex
	blobs  map[string]string // key -> encrypted (or plain) JSON
	crypto *cryptor          // nil => no at-rest encryption (ENCRYPTION_KEY unset)
}

// NewMemoryTokenStore builds a MemoryTokenStore, wiring AES-GCM from
// ENCRYPTION_KEY when present.
func NewMemoryTokenStore() *MemoryTokenStore {
	s := &MemoryTokenStore{blobs: map[string]string{}}
	if key := encryptionKeyFromEnv(); len(key) > 0 {
		if c, err := newCryptor(key); err == nil {
			s.crypto = c
		}
	}
	return s
}

func tokenKey(userID, server string) string { return userID + "\x00" + server }

func (s *MemoryTokenStore) Get(_ context.Context, userID, server string) (*Token, error) {
	s.mu.RLock()
	blob, ok := s.blobs[tokenKey(userID, server)]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNoToken
	}
	raw := []byte(blob)
	if s.crypto != nil {
		dec, err := s.crypto.open(blob)
		if err != nil {
			return nil, fmt.Errorf("mcp: decrypt token: %w", err)
		}
		raw = dec
	}
	var tok Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal token: %w", err)
	}
	return &tok, nil
}

func (s *MemoryTokenStore) Put(_ context.Context, userID, server string, tok *Token) error {
	raw, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	blob := string(raw)
	if s.crypto != nil {
		sealed, err := s.crypto.seal(raw)
		if err != nil {
			return fmt.Errorf("mcp: encrypt token: %w", err)
		}
		blob = sealed
	}
	s.mu.Lock()
	s.blobs[tokenKey(userID, server)] = blob
	s.mu.Unlock()
	return nil
}

func (s *MemoryTokenStore) Delete(_ context.Context, userID, server string) error {
	s.mu.Lock()
	delete(s.blobs, tokenKey(userID, server))
	s.mu.Unlock()
	return nil
}

var _ TokenStore = (*MemoryTokenStore)(nil)
