package handler

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/store"
)

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
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHandleFetch(t *testing.T) {
	dir := setupContentDir(t, map[string]string{
		"hello.md":            "# Hello World\n",
		"with-frontmatter.md": "---\nversion: 5\nauthor: Fritz\n---\n# Doc\n",
	})
	h := &Handler{ContentDir: dir}

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

	t.Run("file with existing frontmatter", func(t *testing.T) {
		stream := newMockStream("FETCH /with-frontmatter.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if resp.Metadata["version"] != "5" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "5")
		}
		if strings.Contains(resp.Body, "---") {
			t.Error("body should not contain frontmatter delimiters")
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
		// Paths with .. segments are rejected outright as defense-in-depth.
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
	dir := setupContentDir(t, map[string]string{
		"hello.md": "# Hello\n",
	})
	h := &Handler{ContentDir: dir}

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
	dir := setupContentDir(t, map[string]string{
		"hello.md": "# Hello World\n",
	})
	h := &Handler{ContentDir: dir}

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
	dir := setupContentDir(t, map[string]string{
		"hello.md": "# Hello World\n",
	})
	h := &Handler{ContentDir: dir}

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

	// Create content directory with a symlink pointing outside.
	dir := setupContentDir(t, map[string]string{
		"public.md": "# Public\n",
	})
	symlinkPath := filepath.Join(dir, "evil.md")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	h := &Handler{ContentDir: dir}

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
	dir := setupContentDir(t, map[string]string{
		"index.md":          "# Index\n",
		"about.md":          "# About\n",
		"docs/guide.md":     "# Guide\n",
		"docs/reference.md": "# Reference\n",
		".hidden":           "secret\n",
	})
	h := &Handler{ContentDir: dir}

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
	dir := setupContentDir(t, map[string]string{
		"hello.md":      "# Hello\n",
		"docs/guide.md": "# Guide\n",
	})
	h := &Handler{ContentDir: dir}

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
	dir := setupContentDir(t, map[string]string{
		"safe.md": "# Safe\n",
	})
	h := &Handler{ContentDir: dir}

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
	// Create a temp dir and work from inside it.
	tmpDir := t.TempDir()
	contentDir := filepath.Join(tmpDir, "site")
	os.MkdirAll(filepath.Join(contentDir, "docs"), 0o755)
	os.WriteFile(filepath.Join(contentDir, "page.md"), []byte("# Page\n"), 0o644)
	os.WriteFile(filepath.Join(contentDir, "docs/guide.md"), []byte("# Guide\n"), 0o644)

	// Use a relative path for ContentDir.
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWd)
	os.Chdir(tmpDir)

	h := &Handler{ContentDir: "./site"}

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
	// Create actual content directory.
	actualDir := t.TempDir()
	os.MkdirAll(filepath.Join(actualDir, "docs"), 0o755)
	os.WriteFile(filepath.Join(actualDir, "file.md"), []byte("# Content\n"), 0o644)
	os.WriteFile(filepath.Join(actualDir, "docs/guide.md"), []byte("# Guide\n"), 0o644)

	// Symlink another path to it.
	symlinkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(actualDir, symlinkDir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	h := &Handler{ContentDir: symlinkDir}

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
	dir := setupContentDir(t, map[string]string{
		"doc.md": "# Current\n",
	})
	// Create versioned files.
	versionsDir := filepath.Join(dir, "versions")
	os.Mkdir(versionsDir, 0o755)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v1"), []byte("# V1\n"), 0o644)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v2"), []byte("# V2\n"), 0o644)

	h := &Handler{
		ContentDir: dir,
		Store:      store.New(dir),
	}

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
		// These version files were manually placed without store frontmatter,
		// so chain-valid will be false (v2 has no previous-hash).
		if resp.Metadata["chain-valid"] != "false" {
			t.Errorf("chain-valid: got %q, want %q", resp.Metadata["chain-valid"], "false")
		}
	})

	t.Run("flat file single version", func(t *testing.T) {
		flatDir := setupContentDir(t, map[string]string{
			"flat.md": "# Flat\n",
		})
		flatH := &Handler{
			ContentDir: flatDir,
			Store:      store.New(flatDir),
		}

		stream := newMockStream("VERSIONS /flat.md\n")
		flatH.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if resp.Metadata["total"] != "1" {
			t.Errorf("total: got %q, want %q", resp.Metadata["total"], "1")
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
		noStoreH := &Handler{ContentDir: dir}

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
	dir := setupContentDir(t, map[string]string{
		"doc.md": "# Current\n",
	})
	versionsDir := filepath.Join(dir, "versions")
	os.Mkdir(versionsDir, 0o755)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v1"), []byte("# Version One\n"), 0o644)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v2"), []byte("# Version Two\n"), 0o644)

	h := &Handler{
		ContentDir: dir,
		Store:      store.New(dir),
	}

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

	t.Run("version path without store falls through to normal fetch", func(t *testing.T) {
		noStoreH := &Handler{ContentDir: dir}

		// Without a store, /doc.md/v1 is treated as a regular path.
		// Result varies by OS: "not-found" or "server-error" depending on
		// how the OS handles stat on a path through a regular file.
		stream := newMockStream("FETCH /doc.md/v1\n")
		noStoreH.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status == protocol.StatusOK {
			t.Error("expected error status, got ok")
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

	h := &Handler{ContentDir: dir, Store: s}

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
		os.WriteFile(v1Path, []byte("# TAMPERED\n"), 0o644)

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

func TestHandleWrite(t *testing.T) {
	t.Run("creates new document", func(t *testing.T) {
		dir := t.TempDir()
		h := &Handler{ContentDir: dir, Store: store.New(dir)}

		stream := newMockStream("WRITE /new.md\n# Hello\n")
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
		dir := setupContentDir(t, map[string]string{
			"doc.md": "# Original\n",
		})
		h := &Handler{ContentDir: dir, Store: store.New(dir)}

		stream := newMockStream("WRITE /doc.md\n# Updated\n")
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
		dir := setupContentDir(t, map[string]string{"doc.md": "# Doc\n"})
		h := &Handler{ContentDir: dir}

		stream := newMockStream("WRITE /doc.md\n# New\n")
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
		h := &Handler{ContentDir: dir, Store: store.New(dir)}

		stream := newMockStream("WRITE /../../etc/passwd\n# evil\n")
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
