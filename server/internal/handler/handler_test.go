package handler

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/auth"
	"github.com/latebit/demarkus/server/internal/store"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// mockStream implements handler.Stream for testing.
type mockStream struct {
	io.Reader
	output bytes.Buffer
	closed bool
}

func (m *mockStream) Write(p []byte) (int, error) { return m.output.Write(p) }
func (m *mockStream) Close() error                { m.closed = true; return nil }

func newMockStream(request string) *mockStream {
	return &mockStream{Reader: strings.NewReader(request)}
}

func setupContentDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// setupVersionedDir creates a content directory and writes files through the
// store so they have proper version history. Returns the dir and store.
func setupVersionedDir(t *testing.T, files map[string]string) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s := store.New(dir)
	for name, content := range files {
		if _, err := s.Write("/"+name, []byte(content)); err != nil {
			t.Fatalf("setupVersionedDir: write %s: %v", name, err)
		}
	}
	return dir, s
}

func TestHandleFetch(t *testing.T) {
	dir, s := setupVersionedDir(t, map[string]string{
		"hello.md": "# Hello World\n",
	})
	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	t.Run("existing file", func(t *testing.T) {
		stream := newMockStream("FETCH /hello.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "# Hello World") {
			t.Errorf("body missing content: %q", resp.Body)
		}
		if resp.Metadata["version"] != "1" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "1")
		}
		if !stream.closed {
			t.Error("stream not closed")
		}
	})

	t.Run("flat file not served", func(t *testing.T) {
		flatDir := setupContentDir(t, map[string]string{
			"flat.md": "# Flat\n",
		})
		flatH := &Handler{ContentDir: flatDir, Store: store.New(flatDir), Logger: discardLogger}

		stream := newMockStream("FETCH /flat.md\n")
		flatH.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("not found", func(t *testing.T) {
		stream := newMockStream("FETCH /nonexistent.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("path traversal blocked", func(t *testing.T) {
		stream := newMockStream("FETCH /../../etc/passwd\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("unsupported verb", func(t *testing.T) {
		stream := newMockStream("DELETE /hello.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusServerError {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusServerError)
		}
	})
}

func TestHealthCheck(t *testing.T) {
	dir, s := setupVersionedDir(t, map[string]string{
		"hello.md": "# Hello\n",
	})
	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	stream := newMockStream("FETCH /health\n")
	h.HandleStream(stream)

	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Status != protocol.StatusOK {
		t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
	}
	if !strings.Contains(resp.Body, "Server is healthy") {
		t.Errorf("body missing health message: %q", resp.Body)
	}
}

func TestEtagInResponse(t *testing.T) {
	dir, s := setupVersionedDir(t, map[string]string{
		"hello.md": "# Hello World\n",
	})
	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	stream := newMockStream("FETCH /hello.md\n")
	h.HandleStream(stream)

	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Metadata["etag"] == "" {
		t.Error("expected etag in response metadata")
	}
	if len(resp.Metadata["etag"]) != 64 {
		t.Errorf("etag should be 64-char hex SHA-256, got %q", resp.Metadata["etag"])
	}
}

func TestConditionalFetch(t *testing.T) {
	dir, s := setupVersionedDir(t, map[string]string{
		"hello.md": "# Hello World\n",
	})
	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	// First fetch to get etag and modified time.
	stream := newMockStream("FETCH /hello.md\n")
	h.HandleStream(stream)

	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	etag := resp.Metadata["etag"]
	modified := resp.Metadata["modified"]

	t.Run("if-none-match hit", func(t *testing.T) {
		req := "FETCH /hello.md\n---\nif-none-match: " + etag + "\n---\n"
		stream := newMockStream(req)
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotModified {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotModified)
		}
		if resp.Body != "" {
			t.Errorf("not-modified should have no body, got %q", resp.Body)
		}
	})

	t.Run("if-none-match miss", func(t *testing.T) {
		req := "FETCH /hello.md\n---\nif-none-match: stale-etag\n---\n"
		stream := newMockStream(req)
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "# Hello World") {
			t.Error("expected full body on etag miss")
		}
	})

	t.Run("if-modified-since not modified", func(t *testing.T) {
		req := "FETCH /hello.md\n---\nif-modified-since: " + modified + "\n---\n"
		stream := newMockStream(req)
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotModified {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotModified)
		}
	})

	t.Run("if-modified-since stale", func(t *testing.T) {
		req := "FETCH /hello.md\n---\nif-modified-since: 2000-01-01T00:00:00Z\n---\n"
		stream := newMockStream(req)
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})
}

func TestSymlinkEscape(t *testing.T) {
	// Create a file outside the content directory.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("SECRET DATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create content directory with a versioned file and a symlink pointing outside.
	dir, s := setupVersionedDir(t, map[string]string{
		"public.md": "# Public\n",
	})
	symlinkPath := filepath.Join(dir, "evil.md")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	t.Run("symlink escape blocked", func(t *testing.T) {
		stream := newMockStream("FETCH /evil.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status == protocol.StatusOK {
			t.Fatal("SECURITY: symlink escape was not blocked")
		}
		if strings.Contains(resp.Body, "SECRET") {
			t.Fatal("SECURITY: secret data leaked through symlink")
		}
	})

	t.Run("symlink directory escape blocked", func(t *testing.T) {
		// Symlink a directory
		symlinkDir := filepath.Join(dir, "escaped")
		if err := os.Symlink(outsideDir, symlinkDir); err != nil {
			t.Skipf("cannot create symlink: %v", err)
		}

		stream := newMockStream("LIST /escaped/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status == protocol.StatusOK {
			t.Fatal("SECURITY: symlink directory escape was not blocked")
		}
	})
}

func TestHandleList(t *testing.T) {
	dir := t.TempDir()
	s := store.New(dir)
	for _, f := range []struct{ path, content string }{
		{"/index.md", "# Index\n"},
		{"/about.md", "# About\n"},
		{"/docs/guide.md", "# Guide\n"},
		{"/docs/reference.md", "# Reference\n"},
	} {
		if _, err := s.Write(f.path, []byte(f.content)); err != nil {
			t.Fatalf("write %s: %v", f.path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	t.Run("list root directory", func(t *testing.T) {
		stream := newMockStream("LIST /\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "[index.md]") {
			t.Error("body should list index.md")
		}
		if !strings.Contains(resp.Body, "[about.md]") {
			t.Error("body should list about.md")
		}
		if !strings.Contains(resp.Body, "[docs/]") {
			t.Error("body should list docs/ directory")
		}
		if strings.Contains(resp.Body, ".hidden") {
			t.Error("body should not list hidden files")
		}
	})

	t.Run("list subdirectory", func(t *testing.T) {
		stream := newMockStream("LIST /docs/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "[guide.md]") {
			t.Error("body should list guide.md")
		}
		if !strings.Contains(resp.Body, "[reference.md]") {
			t.Error("body should list reference.md")
		}
		if resp.Metadata["entries"] != "2" {
			t.Errorf("entries: got %q, want %q", resp.Metadata["entries"], "2")
		}
	})

	t.Run("list nonexistent directory", func(t *testing.T) {
		stream := newMockStream("LIST /nope/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("list a file not a directory", func(t *testing.T) {
		stream := newMockStream("LIST /index.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("path traversal blocked in list", func(t *testing.T) {
		// Paths with .. segments are rejected outright as defense-in-depth.
		stream := newMockStream("LIST /../../\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})
}

func TestMultipleLeadingSlashes(t *testing.T) {
	dir := t.TempDir()
	s := store.New(dir)
	if _, err := s.Write("/hello.md", []byte("# Hello\n")); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if _, err := s.Write("/docs/guide.md", []byte("# Guide\n")); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	fetchPaths := []string{"///hello.md", "//hello.md", "////hello.md"}
	for _, p := range fetchPaths {
		t.Run("FETCH "+p, func(t *testing.T) {
			stream := newMockStream("FETCH " + p + "\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != protocol.StatusOK {
				t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
			}
		})
	}

	listPaths := []string{"///docs/", "//docs/", "////docs/"}
	for _, p := range listPaths {
		t.Run("LIST "+p, func(t *testing.T) {
			stream := newMockStream("LIST " + p + "\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != protocol.StatusOK {
				t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
			}
		})
	}
}

func TestDeeplyNestedTraversal(t *testing.T) {
	dir, s := setupVersionedDir(t, map[string]string{
		"safe.md": "# Safe\n",
	})
	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	paths := []string{
		"/a/../../b/../../etc/passwd",
		"/a/b/c/../../../../etc/passwd",
		"/../../../../../../../etc/passwd",
	}
	for _, p := range paths {
		t.Run("FETCH "+p, func(t *testing.T) {
			stream := newMockStream("FETCH " + p + "\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status == protocol.StatusOK {
				t.Fatalf("SECURITY: path traversal not blocked for %q", p)
			}
		})
		t.Run("LIST "+p, func(t *testing.T) {
			stream := newMockStream("LIST " + p + "\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status == protocol.StatusOK {
				t.Fatalf("SECURITY: path traversal not blocked for %q", p)
			}
		})
	}
}

func TestRelativeContentDir(t *testing.T) {
	// Create a temp dir, write versioned files, then use a relative path.
	tmpDir := t.TempDir()
	contentDir := filepath.Join(tmpDir, "site")
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := store.New(contentDir)
	if _, err := s.Write("/page.md", []byte("# Page\n")); err != nil {
		t.Fatalf("write page: %v", err)
	}
	if _, err := s.Write("/docs/guide.md", []byte("# Guide\n")); err != nil {
		t.Fatalf("write guide: %v", err)
	}

	// Use a relative path for ContentDir.
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	relStore := store.New("./site")
	h := &Handler{ContentDir: "./site", Store: relStore, Logger: discardLogger}

	t.Run("fetch works with relative content dir", func(t *testing.T) {
		stream := newMockStream("FETCH /page.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("traversal blocked with relative content dir", func(t *testing.T) {
		stream := newMockStream("FETCH /../../etc/passwd\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status == protocol.StatusOK {
			t.Fatal("SECURITY: traversal not blocked with relative ContentDir")
		}
	})

	t.Run("list works with relative content dir", func(t *testing.T) {
		stream := newMockStream("LIST /docs/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("list traversal blocked with relative content dir", func(t *testing.T) {
		stream := newMockStream("LIST /../../\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		// Traversal resolves to content root, which is safe — but must not escape it.
		if resp.Status == protocol.StatusOK {
			// If ok, verify it listed the content dir (not something outside it).
			if !strings.Contains(resp.Body, "page.md") {
				t.Fatal("SECURITY: LIST traversal escaped relative ContentDir")
			}
		}
	})
}

func TestContentDirAsSymlink(t *testing.T) {
	// Create actual content directory with versioned files.
	actualDir := t.TempDir()
	s := store.New(actualDir)
	if _, err := s.Write("/file.md", []byte("# Content\n")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := s.Write("/docs/guide.md", []byte("# Guide\n")); err != nil {
		t.Fatalf("write guide: %v", err)
	}

	// Symlink another path to it.
	symlinkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(actualDir, symlinkDir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	symlinkStore := store.New(symlinkDir)
	h := &Handler{ContentDir: symlinkDir, Store: symlinkStore, Logger: discardLogger}

	t.Run("fetch through symlinked content dir", func(t *testing.T) {
		stream := newMockStream("FETCH /file.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("traversal blocked with symlinked content dir", func(t *testing.T) {
		stream := newMockStream("FETCH /../../../etc/passwd\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status == protocol.StatusOK {
			t.Fatal("SECURITY: traversal not blocked when ContentDir is symlink")
		}
	})

	t.Run("list through symlinked content dir", func(t *testing.T) {
		stream := newMockStream("LIST /docs/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("list traversal blocked with symlinked content dir", func(t *testing.T) {
		stream := newMockStream("LIST /../../../\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status == protocol.StatusOK {
			if !strings.Contains(resp.Body, "file.md") {
				t.Fatal("SECURITY: LIST traversal escaped symlinked ContentDir")
			}
		}
	})
}

func TestHandleVersions(t *testing.T) {
	dir, s := setupVersionedDir(t, map[string]string{
		"doc.md": "# V1\n",
	})
	// Write a second version.
	if _, err := s.Write("/doc.md", []byte("# V2\n")); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	t.Run("version history", func(t *testing.T) {
		stream := newMockStream("VERSIONS /doc.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if resp.Metadata["total"] != "2" {
			t.Errorf("total: got %q, want %q", resp.Metadata["total"], "2")
		}
		if resp.Metadata["current"] != "2" {
			t.Errorf("current: got %q, want %q", resp.Metadata["current"], "2")
		}
		if !strings.Contains(resp.Body, "v1") || !strings.Contains(resp.Body, "v2") {
			t.Errorf("body should list both versions: %q", resp.Body)
		}
		if resp.Metadata["chain-valid"] != "true" {
			t.Errorf("chain-valid: got %q, want %q", resp.Metadata["chain-valid"], "true")
		}
	})

	t.Run("flat file not found", func(t *testing.T) {
		flatDir := setupContentDir(t, map[string]string{
			"flat.md": "# Flat\n",
		})
		flatH := &Handler{
			ContentDir: flatDir,
			Store:      store.New(flatDir),
			Logger:     discardLogger,
		}

		stream := newMockStream("VERSIONS /flat.md\n")
		flatH.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("not found", func(t *testing.T) {
		stream := newMockStream("VERSIONS /missing.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("no store configured", func(t *testing.T) {
		noStoreH := &Handler{ContentDir: dir, Logger: discardLogger}

		stream := newMockStream("VERSIONS /doc.md\n")
		noStoreH.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusServerError {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusServerError)
		}
	})
}

func TestFetchVersion(t *testing.T) {
	dir, s := setupVersionedDir(t, map[string]string{
		"doc.md": "# Version One\n",
	})
	if _, err := s.Write("/doc.md", []byte("# Version Two\n")); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	t.Run("fetch specific version", func(t *testing.T) {
		stream := newMockStream("FETCH /doc.md/v1\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "# Version One") {
			t.Errorf("body should contain v1 content: %q", resp.Body)
		}
		if resp.Metadata["version"] != "1" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "1")
		}
		if resp.Metadata["current-version"] != "2" {
			t.Errorf("current-version: got %q, want %q", resp.Metadata["current-version"], "2")
		}
	})

	t.Run("fetch nonexistent version", func(t *testing.T) {
		stream := newMockStream("FETCH /doc.md/v99\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("fetch version of nonexistent doc", func(t *testing.T) {
		stream := newMockStream("FETCH /missing.md/v1\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})
}

func TestVersionsChainValid(t *testing.T) {
	dir := t.TempDir()
	s := store.New(dir)

	// Write versions through the store to get proper hash chain.
	if _, err := s.Write("/doc.md", []byte("# V1\n")); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if _, err := s.Write("/doc.md", []byte("# V2\n")); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger}

	t.Run("valid chain", func(t *testing.T) {
		stream := newMockStream("VERSIONS /doc.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Metadata["chain-valid"] != "true" {
			t.Errorf("chain-valid: got %q, want %q", resp.Metadata["chain-valid"], "true")
		}
	})

	t.Run("tampered chain", func(t *testing.T) {
		// Corrupt v1 on disk.
		v1Path := filepath.Join(dir, "versions", "doc.md.v1")
		if err := os.WriteFile(v1Path, []byte("# TAMPERED\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		stream := newMockStream("VERSIONS /doc.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Metadata["chain-valid"] != "false" {
			t.Errorf("chain-valid: got %q, want %q", resp.Metadata["chain-valid"], "false")
		}
		if resp.Metadata["chain-error"] == "" {
			t.Error("expected chain-error in metadata")
		}
	})
}

func TestHandlePublish(t *testing.T) {
	// A permissive token store for tests that need to exercise write logic.
	const testSecret = "test-publish-secret"
	publishTokenStore := auth.NewTokenStore(map[string]auth.Token{
		auth.HashToken(testSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})
	authMeta := "---\nauth: " + testSecret + "\n---\n"

	t.Run("creates new document", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return publishTokenStore }}

		stream := newMockStream("PUBLISH /new.md\n" + authMeta + "# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
		if resp.Metadata["version"] != "1" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "1")
		}
		if resp.Metadata["modified"] == "" {
			t.Error("expected modified in response metadata")
		}
	})

	t.Run("creates new version of existing document", func(t *testing.T) {
		dir := t.TempDir()
		s := store.New(dir)
		if _, err := s.Write("/doc.md", []byte("# Original\n")); err != nil {
			t.Fatalf("write v1: %v", err)
		}
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return publishTokenStore }}

		stream := newMockStream("PUBLISH /doc.md\n" + authMeta + "# Updated\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
		if resp.Metadata["version"] != "2" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "2")
		}
	})

	t.Run("no store configured", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return publishTokenStore }}

		stream := newMockStream("PUBLISH /doc.md\n" + authMeta + "# New\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusServerError {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusServerError)
		}
	})

	t.Run("path traversal blocked", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return publishTokenStore }}

		stream := newMockStream("PUBLISH /../../etc/passwd\n" + authMeta + "# evil\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status == protocol.StatusCreated {
			t.Error("SECURITY: path traversal not blocked")
		}
	})
}

func TestHandlePublishAuth(t *testing.T) {
	// Raw secrets used in requests. Store keys are their hashes.
	const (
		writerSecret   = "writer-secret"
		readonlySecret = "readonly-secret"
	)

	ts := auth.NewTokenStore(map[string]auth.Token{
		auth.HashToken(writerSecret): {
			Paths:      []string{"/docs/*"},
			Operations: []string{"publish"},
		},
		auth.HashToken(readonlySecret): {
			Paths:      []string{"/*"},
			Operations: []string{"read"},
		},
	})

	t.Run("no token store denies publishing", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger}

		stream := newMockStream("PUBLISH /doc.md\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotPermitted {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotPermitted)
		}
	})

	t.Run("missing token returns unauthorized", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("PUBLISH /docs/test.md\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusUnauthorized {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusUnauthorized)
		}
	})

	t.Run("invalid token returns unauthorized", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("PUBLISH /docs/test.md\n---\nauth: wrong-secret\n---\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusUnauthorized {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusUnauthorized)
		}
	})

	t.Run("valid token wrong path returns not-permitted", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("PUBLISH /private/secret.md\n---\nauth: " + writerSecret + "\n---\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotPermitted {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotPermitted)
		}
	})

	t.Run("valid token wrong operation returns not-permitted", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("PUBLISH /docs/test.md\n---\nauth: " + readonlySecret + "\n---\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotPermitted {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotPermitted)
		}
	})

	t.Run("valid token correct path succeeds", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir), Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("PUBLISH /docs/test.md\n---\nauth: " + writerSecret + "\n---\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
	})
}

func TestParseVersionPath(t *testing.T) {
	tests := []struct {
		path    string
		base    string
		version int
	}{
		{"/doc.md/v1", "/doc.md", 1},
		{"/doc.md/v42", "/doc.md", 42},
		{"/docs/guide.md/v3", "/docs/guide.md", 3},
		{"/doc.md", "/doc.md", 0},
		{"/doc.md/v0", "/doc.md/v0", 0},
		{"/doc.md/v-1", "/doc.md/v-1", 0},
		{"/doc.md/notversion", "/doc.md/notversion", 0},
		{"/v1", "/v1", 0},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			base, version := parseVersionPath(tt.path)
			if base != tt.base || version != tt.version {
				t.Errorf("parseVersionPath(%q) = (%q, %d), want (%q, %d)",
					tt.path, base, version, tt.base, tt.version)
			}
		})
	}
}

func TestHandleArchive(t *testing.T) {
	writerSecret := "test-secret-key"
	ts := auth.NewTokenStore(map[string]auth.Token{
		auth.HashToken(writerSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})

	t.Run("archive not found", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("ARCHIVE /missing.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("archive requires auth", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("ARCHIVE /doc.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusUnauthorized {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusUnauthorized)
		}
	})

	t.Run("archive with valid token succeeds", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("ARCHIVE /doc.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if resp.Metadata["archived"] != "true" {
			t.Errorf("archived metadata: got %q, want %q", resp.Metadata["archived"], "true")
		}
	})

	t.Run("fetch archived document returns archived status", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		// Archive the document
		stream := newMockStream("ARCHIVE /doc.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		// Try to fetch it
		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusArchived {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusArchived)
		}
	})

	t.Run("publish with body to archived document fails", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		// Archive the document
		stream := newMockStream("ARCHIVE /doc.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		// Try to publish to archived document
		stream = newMockStream("PUBLISH /doc.md\n---\nauth: " + writerSecret + "\n---\n# New Content\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusArchived {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusArchived)
		}
	})

	t.Run("publish with empty body unarchives document", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		// Archive the document
		stream := newMockStream("ARCHIVE /doc.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		// Publish with empty body to unarchive
		stream = newMockStream("PUBLISH /doc.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}

		// Now FETCH should succeed
		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)

		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("fetch specific version of archived document succeeds", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		// Archive the document
		stream := newMockStream("ARCHIVE /doc.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		// Fetch specific version should still work
		stream = newMockStream("FETCH /doc.md/v1\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("publish with body to active document still works", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + writerSecret + "\n---\n# New Content\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
		if resp.Metadata["version"] != "2" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "2")
		}
	})

	t.Run("publish with empty body to active document is no-op", func(t *testing.T) {
		dir, s := setupVersionedDir(t, map[string]string{"doc.md": "# Content\n"})
		h := &Handler{ContentDir: dir, Store: s, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }}

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + writerSecret + "\n---\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if resp.Metadata["version"] != "1" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "1")
		}
	})
}
