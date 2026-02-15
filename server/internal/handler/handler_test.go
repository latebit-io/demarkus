package handler

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit/demarkus/protocol"
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

	t.Run("path traversal contained", func(t *testing.T) {
		// ../../etc/passwd resolves inside the content dir (filepath.Join handles this).
		// The file doesn't exist there, so it returns not-found — path cannot escape.
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

	t.Run("path traversal resolves to root", func(t *testing.T) {
		// /../../ resolves to / via filepath.Clean, which maps to the content root.
		// This is safe — the path cannot escape the content directory.
		stream := newMockStream("LIST /../../\n")
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
