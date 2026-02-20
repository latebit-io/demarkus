// Package auth provides capability-based token authentication for the Mark Protocol.
//
// Tokens are loaded from a TOML file at startup. Each token grants specific
// operations (read, write) on specific path patterns. Tokens are capability-based:
// they grant what you can do, not who you are.
//
// This design supports both human and AI/agent access — tokens can be scoped
// to specific paths and operations, enabling fine-grained programmatic access
// without CAPTCHAs or browser-dependent flows.
//
// TOML format:
//
//	[tokens]
//	"sha256-abc123..." = { paths = ["/docs/*"], operations = ["write"] }
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Token represents a single capability token's permissions.
type Token struct {
	Paths      []string `toml:"paths"`
	Operations []string `toml:"operations"`
	// TODO: enforce token expiration. The field is loaded from TOML but
	// not checked by Authorize. Future increment.
	Expires string `toml:"expires"`
}

// tokensFile is the top-level TOML structure.
type tokensFile struct {
	Tokens map[string]Token `toml:"tokens"`
}

// TokenStore holds loaded tokens and provides authorization checks.
type TokenStore struct {
	tokens map[string]Token
}

// Sentinel errors for authorization results.
var (
	ErrNoToken      = errors.New("no auth token provided")
	ErrInvalidToken = errors.New("invalid auth token")
	ErrNotPermitted = errors.New("insufficient permissions")
)

// LoadTokens reads a TOML tokens file and returns a TokenStore.
func LoadTokens(path string) (*TokenStore, error) {
	var tf tokensFile
	if _, err := toml.DecodeFile(path, &tf); err != nil {
		return nil, fmt.Errorf("load tokens file %q: %w", path, err)
	}
	if tf.Tokens == nil {
		tf.Tokens = make(map[string]Token)
	}
	return &TokenStore{tokens: tf.Tokens}, nil
}

// NewTokenStore creates a TokenStore from an in-memory token map.
func NewTokenStore(tokens map[string]Token) *TokenStore {
	return &TokenStore{tokens: tokens}
}

// HashToken returns the SHA-256 hash of a raw token in the format "sha256-<hex>".
// The TOML tokens file stores these hashes as keys. Clients send the raw secret,
// and the server hashes it before lookup — so the tokens file never contains
// plaintext secrets.
func HashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return "sha256-" + hex.EncodeToString(h[:])
}

// Authorize checks whether the given raw token is allowed to perform the given
// operation on the given path. The raw token is hashed before lookup.
//
// Returns nil if authorized, or one of the sentinel errors:
//   - ErrNoToken: token is empty
//   - ErrInvalidToken: token not recognized
//   - ErrNotPermitted: token exists but lacks permission for this path/operation
//
// TODO: timestamp validation for replay protection (±5 min window, nonce per token).
// TODO: per-document ACLs (.mark-acl files).
// TODO: rate limiting for public-facing servers.
func (ts *TokenStore) Authorize(token, reqPath, operation string) error {
	if token == "" {
		return ErrNoToken
	}
	hashed := HashToken(token)
	t, ok := ts.tokens[hashed]
	if !ok {
		return ErrInvalidToken
	}
	if !hasOperation(t.Operations, operation) {
		return ErrNotPermitted
	}
	if !matchesAnyPath(t.Paths, reqPath) {
		return ErrNotPermitted
	}
	return nil
}

func hasOperation(ops []string, target string) bool {
	for _, op := range ops {
		if op == target {
			return true
		}
	}
	return false
}

// matchesAnyPath checks if reqPath matches any of the glob patterns.
// Uses filepath.Match which supports single-level * and ? wildcards.
// TODO: support recursive glob (**) for matching nested paths.
func matchesAnyPath(patterns []string, reqPath string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, reqPath); matched {
			return true
		}
	}
	return false
}
