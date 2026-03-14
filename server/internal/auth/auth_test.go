package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadTokens(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		data := `[tokens.writer]
hash = "sha256-abc"
paths = ["/docs/*"]
operations = ["publish"]

[tokens.readonly]
hash = "sha256-readonly"
paths = ["/*"]
operations = ["read"]
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		ts, err := LoadTokens(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ts.tokens) != 2 {
			t.Errorf("token count: got %d, want 2", len(ts.tokens))
		}
		// Verify tokens are keyed by hash.
		tok, ok := ts.tokens["sha256-abc"]
		if !ok {
			t.Fatal("token sha256-abc not found")
		}
		if tok.Label != "writer" {
			t.Errorf("label: got %q, want %q", tok.Label, "writer")
		}
	})

	t.Run("empty tokens section", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		if err := os.WriteFile(path, []byte("[tokens]\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		ts, err := LoadTokens(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ts.tokens) != 0 {
			t.Errorf("token count: got %d, want 0", len(ts.tokens))
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := LoadTokens("/nonexistent/tokens.toml")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("invalid TOML", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		if err := os.WriteFile(path, []byte("not valid toml {{{"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadTokens(path)
		if err == nil {
			t.Fatal("expected error for invalid TOML")
		}
	})

	t.Run("token with expires field", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		data := `[tokens.expiring]
hash = "sha256-expiring"
paths = ["/*"]
operations = ["publish"]
expires = "2026-12-31T23:59:59Z"
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		ts, err := LoadTokens(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		tok := ts.tokens["sha256-expiring"]
		if tok.Expires != "2026-12-31T23:59:59Z" {
			t.Errorf("expires: got %q, want %q", tok.Expires, "2026-12-31T23:59:59Z")
		}
		if tok.expiresAt.IsZero() {
			t.Error("expiresAt should be parsed, got zero")
		}
	})

	t.Run("invalid path pattern", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		data := `[tokens.bad]
hash = "sha256-bad"
paths = ["/docs/[invalid"]
operations = ["publish"]
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadTokens(path)
		if err == nil {
			t.Fatal("expected error for invalid path pattern")
		}
	})

	t.Run("bare double star pattern", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		data := `[tokens.bad]
hash = "sha256-bad"
paths = ["/docs**"]
operations = ["publish"]
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadTokens(path)
		if err == nil {
			t.Fatal("expected error for bare ** without slash delimiters")
		}
	})

	t.Run("multiple double star pattern", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		data := `[tokens.bad]
hash = "sha256-bad"
paths = ["/a/**/b/**/c"]
operations = ["publish"]
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadTokens(path)
		if err == nil {
			t.Fatal("expected error for multiple ** wildcards")
		}
	})

	t.Run("invalid expires format", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tokens.toml")
		data := `[tokens.bad]
hash = "sha256-bad"
paths = ["/*"]
operations = ["publish"]
expires = "not-a-date"
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadTokens(path)
		if err == nil {
			t.Fatal("expected error for invalid expires format")
		}
	})
}

func TestHashToken(t *testing.T) {
	hash := HashToken("my-secret")
	if hash[:7] != "sha256-" {
		t.Errorf("hash prefix: got %q, want sha256- prefix", hash[:7])
	}
	if len(hash) != 7+64 { // "sha256-" + 64 hex chars
		t.Errorf("hash length: got %d, want %d", len(hash), 7+64)
	}
	// Same input produces same hash.
	if HashToken("my-secret") != hash {
		t.Error("HashToken is not deterministic")
	}
	// Different input produces different hash.
	if HashToken("other-secret") == hash {
		t.Error("different inputs produced same hash")
	}
}

func TestAuthorize(t *testing.T) {
	// Raw secrets used by clients.
	const (
		writerSecret    = "writer-secret"
		readwriteSecret = "readwrite-secret"
		readonlySecret  = "readonly-secret"
	)

	// Token store keys are hashes of raw secrets.
	ts := NewTokenStore(map[string]Token{
		HashToken(writerSecret): {
			Label:      "writer",
			Paths:      []string{"/docs/*"},
			Operations: []string{"publish"},
		},
		HashToken(readwriteSecret): {
			Label:      "readwrite",
			Paths:      []string{"/*"},
			Operations: []string{"read", "publish"},
		},
		HashToken(readonlySecret): {
			Label:      "readonly",
			Paths:      []string{"/*"},
			Operations: []string{"read"},
		},
	})

	tests := []struct {
		name      string
		token     string
		path      string
		operation string
		wantLabel string
		wantErr   error
	}{
		{"valid publish", writerSecret, "/docs/test.md", "publish", "writer", nil},
		{"valid readpublish", readwriteSecret, "/anything.md", "publish", "readwrite", nil},
		{"empty token", "", "/docs/test.md", "publish", "", ErrNoToken},
		{"unknown token", "unknown-secret", "/docs/test.md", "publish", "", ErrInvalidToken},
		{"wrong operation", readonlySecret, "/docs/test.md", "publish", "", ErrNotPermitted},
		{"wrong path", writerSecret, "/private/secret.md", "publish", "", ErrNotPermitted},
		{"glob match", writerSecret, "/docs/nested.md", "publish", "writer", nil},
		{"glob no match nested", writerSecret, "/docs/sub/file.md", "publish", "", ErrNotPermitted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, err := ts.Authorize(tt.token, tt.path, tt.operation)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Authorize(%q, %q, %q): got %v, want %v",
					tt.token, tt.path, tt.operation, err, tt.wantErr)
			}
			if label != tt.wantLabel {
				t.Errorf("Authorize(%q, %q, %q): label got %q, want %q",
					tt.token, tt.path, tt.operation, label, tt.wantLabel)
			}
		})
	}
}

func TestAuthorizeExpiration(t *testing.T) {
	const secret = "expiring-secret"

	t.Run("expired token", func(t *testing.T) {
		ts := NewTokenStore(map[string]Token{
			HashToken(secret): {
				Paths:      []string{"/*"},
				Operations: []string{"publish"},
				expiresAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		})
		ts.now = func() time.Time { return time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC) }

		_, err := ts.Authorize(secret, "/doc.md", "publish")
		if !errors.Is(err, ErrTokenExpired) {
			t.Errorf("got %v, want ErrTokenExpired", err)
		}
	})

	t.Run("not yet expired token", func(t *testing.T) {
		ts := NewTokenStore(map[string]Token{
			HashToken(secret): {
				Paths:      []string{"/*"},
				Operations: []string{"publish"},
				expiresAt:  time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
			},
		})
		ts.now = func() time.Time { return time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC) }

		_, err := ts.Authorize(secret, "/doc.md", "publish")
		if err != nil {
			t.Errorf("got %v, want nil", err)
		}
	})

	t.Run("no expiry set", func(t *testing.T) {
		ts := NewTokenStore(map[string]Token{
			HashToken(secret): {
				Paths:      []string{"/*"},
				Operations: []string{"publish"},
				// expiresAt is zero value — no expiry
			},
		})
		ts.now = func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }

		_, err := ts.Authorize(secret, "/doc.md", "publish")
		if err != nil {
			t.Errorf("got %v, want nil", err)
		}
	})
}

func TestAuthorizeRecursiveGlob(t *testing.T) {
	const secret = "recursive-secret"

	ts := NewTokenStore(map[string]Token{
		HashToken(secret): {
			Paths:      []string{"/docs/**"},
			Operations: []string{"publish"},
		},
	})

	tests := []struct {
		name    string
		path    string
		wantErr error
	}{
		{"child path", "/docs/file.md", nil},
		{"nested path", "/docs/sub/file.md", nil},
		{"deeply nested", "/docs/a/b/c/file.md", nil},
		{"wrong prefix", "/other/file.md", ErrNotPermitted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ts.Authorize(secret, tt.path, "publish")
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Authorize(%q, %q): got %v, want %v",
					tt.path, "publish", err, tt.wantErr)
			}
		})
	}
}

func TestMatchesAnyPath(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{"single match", []string{"/docs/*"}, "/docs/file.md", true},
		{"no recursive match", []string{"/docs/*"}, "/docs/sub/file.md", false},
		{"multiple patterns", []string{"/docs/*", "/public/*"}, "/public/page.md", true},
		{"no match", []string{"/private/*"}, "/docs/file.md", false},
		{"wildcard root", []string{"/*"}, "/anything.md", true},
		{"exact match", []string{"/index.md"}, "/index.md", true},
		{"exact no match", []string{"/index.md"}, "/other.md", false},
		{"empty patterns", []string{}, "/docs/file.md", false},
		// Recursive glob (**) tests.
		{"recursive glob matches child", []string{"/docs/**"}, "/docs/file.md", true},
		{"recursive glob matches nested", []string{"/docs/**"}, "/docs/sub/file.md", true},
		{"recursive glob matches deeply nested", []string{"/docs/**"}, "/docs/a/b/c/file.md", true},
		{"recursive glob no match other dir", []string{"/docs/**"}, "/other/file.md", false},
		{"recursive glob no match prefix itself", []string{"/docs/**"}, "/docs", false},
		{"recursive glob root", []string{"/**"}, "/anything.md", true},
		{"recursive glob root nested", []string{"/**"}, "/a/b/c.md", true},
		{"infix glob matches", []string{"/docs/**/file.md"}, "/docs/sub/file.md", true},
		{"infix glob matches deep", []string{"/docs/**/file.md"}, "/docs/a/b/file.md", true},
		{"infix glob no match wrong suffix", []string{"/docs/**/file.md"}, "/docs/sub/other.md", false},
		{"infix glob no match wrong prefix", []string{"/docs/**/file.md"}, "/other/sub/file.md", false},
		{"infix glob no intermediate dir", []string{"/docs/**/file.md"}, "/docs/file.md", true},
		// Multi-segment suffix after **.
		{"infix glob multi-segment suffix", []string{"/docs/**/sub/*.md"}, "/docs/a/sub/x.md", true},
		{"infix glob multi-segment suffix deep", []string{"/docs/**/sub/*.md"}, "/docs/a/b/sub/notes.md", true},
		{"infix glob multi-segment suffix no match", []string{"/docs/**/sub/*.md"}, "/docs/a/other/x.md", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesAnyPath(tt.patterns, tt.path)
			if got != tt.want {
				t.Errorf("matchesAnyPath(%v, %q): got %v, want %v",
					tt.patterns, tt.path, got, tt.want)
			}
		})
	}
}

func TestRequiresReadAuth(t *testing.T) {
	tests := []struct {
		name   string
		tokens map[string]Token
		path   string
		want   bool
	}{
		{
			"path covered by read token",
			map[string]Token{
				"sha256-read": {Paths: []string{"/private/**"}, Operations: []string{"read"}},
			},
			"/private/doc.md",
			true,
		},
		{
			"path not covered by any read token",
			map[string]Token{
				"sha256-read": {Paths: []string{"/private/**"}, Operations: []string{"read"}},
			},
			"/public/doc.md",
			false,
		},
		{
			"publish-only token does not protect reads",
			map[string]Token{
				"sha256-write": {Paths: []string{"/docs/**"}, Operations: []string{"publish"}},
			},
			"/docs/file.md",
			false,
		},
		{
			"no tokens",
			map[string]Token{},
			"/anything.md",
			false,
		},
		{
			"read token with single-level glob",
			map[string]Token{
				"sha256-read": {Paths: []string{"/secret/*"}, Operations: []string{"read"}},
			},
			"/secret/notes.md",
			true,
		},
		{
			"read token does not match nested without recursive glob",
			map[string]Token{
				"sha256-read": {Paths: []string{"/secret/*"}, Operations: []string{"read"}},
			},
			"/secret/sub/notes.md",
			false,
		},
		{
			"mixed tokens only read paths count",
			map[string]Token{
				"sha256-write": {Paths: []string{"/**"}, Operations: []string{"publish"}},
				"sha256-read":  {Paths: []string{"/internal/**"}, Operations: []string{"read"}},
			},
			"/public/doc.md",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := NewTokenStore(tt.tokens)
			got := ts.RequiresReadAuth(tt.path)
			if got != tt.want {
				t.Errorf("RequiresReadAuth(%q): got %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
