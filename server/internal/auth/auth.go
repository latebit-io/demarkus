// Package auth provides capability-based token authentication for the Mark Protocol.
//
// Tokens are loaded from a TOML file at startup. Each token grants specific
// operations (read, publish) on specific path patterns. Tokens are capability-based:
// they grant what you can do, not who you are.
//
// This design supports both human and AI/agent access — tokens can be scoped
// to specific paths and operations, enabling fine-grained programmatic access
// without CAPTCHAs or browser-dependent flows.
//
// TOML format:
//
//	[tokens.fritz-laptop]
//	hash = "sha256-abc123..."
//	paths = ["/docs/*"]
//	operations = ["publish"]
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Token represents a single capability token's permissions.
type Token struct {
	Hash       string    `toml:"hash"`
	Paths      []string  `toml:"paths"`
	Operations []string  `toml:"operations"`
	Label      string    `toml:"-"`       // set from TOML key, not stored in file
	Expires    string    `toml:"expires"` // RFC 3339 timestamp, empty means no expiry
	expiresAt  time.Time // parsed from Expires at load time
}

// tokensFile is the top-level TOML structure.
type tokensFile struct {
	Tokens map[string]Token `toml:"tokens"`
}

// TokenStore holds loaded tokens and provides authorization checks.
type TokenStore struct {
	tokens map[string]Token // keyed by hash for fast lookup
	now    func() time.Time // injectable clock for testing
}

// Sentinel errors for authorization results.
var (
	ErrNoToken      = errors.New("no auth token provided")
	ErrInvalidToken = errors.New("invalid auth token")
	ErrNotPermitted = errors.New("insufficient permissions")
	ErrTokenExpired = errors.New("token has expired")
)

// LoadTokens reads a TOML tokens file and returns a TokenStore.
// The file uses labeled entries where the key is a human-readable label:
//
//	[tokens.fritz-laptop]
//	hash = "sha256-abc123..."
//	paths = ["/docs/*"]
//	operations = ["publish"]
func LoadTokens(filePath string) (*TokenStore, error) {
	var tf tokensFile
	if _, err := toml.DecodeFile(filePath, &tf); err != nil {
		return nil, fmt.Errorf("load tokens file %q: %w", filePath, err)
	}
	if tf.Tokens == nil {
		tf.Tokens = make(map[string]Token)
	}
	// Re-key from label → token to hash → token for fast authorize lookups.
	byHash := make(map[string]Token, len(tf.Tokens))
	for label, tok := range tf.Tokens {
		tok.Label = label
		if tok.Hash == "" {
			return nil, fmt.Errorf("token %q has empty hash", label)
		}
		if tok.Expires != "" {
			t, err := time.Parse(time.RFC3339, tok.Expires)
			if err != nil {
				return nil, fmt.Errorf("token %q has invalid expires %q: %w", label, tok.Expires, err)
			}
			tok.expiresAt = t
		}
		for _, p := range tok.Paths {
			if err := validatePattern(p); err != nil {
				return nil, fmt.Errorf("token %q has invalid path pattern %q: %w", label, p, err)
			}
		}
		if existing, ok := byHash[tok.Hash]; ok {
			return nil, fmt.Errorf("duplicate hash for labels %q and %q", existing.Label, label)
		}
		byHash[tok.Hash] = tok
	}
	return &TokenStore{tokens: byHash, now: time.Now}, nil
}

// NewTokenStore creates a TokenStore from an in-memory token map keyed by hash.
func NewTokenStore(tokens map[string]Token) *TokenStore {
	return &TokenStore{tokens: tokens, now: time.Now}
}

// HashToken returns the SHA-256 hash of a raw token in the format "sha256-<hex>".
// The TOML tokens file stores these hashes. Clients send the raw secret,
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
//   - ErrTokenExpired: token has passed its expiration time
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
	if !t.expiresAt.IsZero() && ts.now().After(t.expiresAt) {
		return ErrTokenExpired
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
	return slices.Contains(ops, target)
}

// matchesAnyPath checks if reqPath matches any of the glob patterns.
// Supports single-level * and ? wildcards via path.Match, plus
// recursive ** wildcards for matching across directory levels:
//   - /docs/**       matches anything under /docs/
//   - /docs/**/a.md  matches /docs/sub/a.md, /docs/a/b/a.md, etc.
//   - /**            matches everything
//
// Only one ** wildcard is supported per pattern.
//
// Uses path.Match (not filepath.Match) because token paths are URL-style
// forward slashes, and filepath.Match behavior varies by OS.
func matchesAnyPath(patterns []string, reqPath string) bool {
	for _, pattern := range patterns {
		if matchPath(pattern, reqPath) {
			return true
		}
	}
	return false
}

// matchPath checks a single pattern against a path. It handles ** globs
// by splitting on /**/ and checking prefix + suffix, falling back to
// path.Match for patterns without **.
// Patterns are validated at load time by validatePattern, so path.Match
// errors are unreachable here and safely ignored.
func matchPath(pattern, reqPath string) bool {
	if !strings.Contains(pattern, "**") {
		matched, _ := path.Match(pattern, reqPath)
		return matched
	}

	// Trailing /** — matches anything under the prefix.
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(reqPath, prefix+"/")
	}

	// Infix /**/ — prefix must match the start, suffix must match a trailing
	// subpath. The suffix can span multiple segments (e.g. /**/sub/*.md).
	if prefix, suffix, ok := strings.Cut(pattern, "/**/"); ok {
		if !strings.HasPrefix(reqPath, prefix+"/") {
			return false
		}
		remaining := reqPath[len(prefix)+1:]
		// Try matching the suffix against every possible tail starting at
		// a segment boundary, so /docs/**/sub/*.md matches /docs/a/sub/x.md.
		for i := range len(remaining) {
			if i > 0 && remaining[i-1] != '/' {
				continue
			}
			if matched, _ := path.Match(suffix, remaining[i:]); matched {
				return true
			}
		}
		return false
	}

	return false
}

// validatePattern checks that a glob pattern has valid syntax. At most one
// ** wildcard is supported, and it must appear as /** (trailing) or /**/
// (infix). Bare ** without surrounding slashes is rejected.
func validatePattern(pattern string) error {
	if n := strings.Count(pattern, "**"); n > 1 {
		return fmt.Errorf("only one ** wildcard is supported per pattern")
	} else if n == 1 {
		// The single ** must be slash-delimited: /**/ or /**.
		stripped := strings.ReplaceAll(pattern, "/**/", "/")
		stripped = strings.TrimSuffix(stripped, "/**")
		if strings.Contains(stripped, "**") {
			return fmt.Errorf("** must be delimited by slashes (use /** or /**/)")
		}
	}
	// Validate the non-** portions with path.Match.
	clean := strings.ReplaceAll(pattern, "**", "placeholder")
	_, err := path.Match(clean, clean)
	return err
}
