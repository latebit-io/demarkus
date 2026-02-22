package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
			Paths:      []string{"/docs/*"},
			Operations: []string{"publish"},
		},
		HashToken(readwriteSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"read", "publish"},
		},
		HashToken(readonlySecret): {
			Paths:      []string{"/*"},
			Operations: []string{"read"},
		},
	})

	tests := []struct {
		name      string
		token     string
		path      string
		operation string
		wantErr   error
	}{
		{"valid publish", writerSecret, "/docs/test.md", "publish", nil},
		{"valid readpublish", readwriteSecret, "/anything.md", "publish", nil},
		{"empty token", "", "/docs/test.md", "publish", ErrNoToken},
		{"unknown token", "unknown-secret", "/docs/test.md", "publish", ErrInvalidToken},
		{"wrong operation", readonlySecret, "/docs/test.md", "publish", ErrNotPermitted},
		{"wrong path", writerSecret, "/private/secret.md", "publish", ErrNotPermitted},
		{"glob match", writerSecret, "/docs/nested.md", "publish", nil},
		{"glob no match nested", writerSecret, "/docs/sub/file.md", "publish", ErrNotPermitted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ts.Authorize(tt.token, tt.path, tt.operation)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Authorize(%q, %q, %q): got %v, want %v",
					tt.token, tt.path, tt.operation, err, tt.wantErr)
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
