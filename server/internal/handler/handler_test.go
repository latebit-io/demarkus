package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
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

func TestHandleFetch(t *testing.T) { forEachBackend(t, testHandleFetch) }

func testHandleFetch(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"hello.md": "# Hello World\n",
	})
	h := newHandler(b, nil)

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

	t.Run("content-hash in response", func(t *testing.T) {
		stream := newMockStream("FETCH /hello.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}

		// content-hash should be sha256 of the body (not the stored content with frontmatter)
		hash := sha256.Sum256([]byte(resp.Body))
		want := "sha256-" + hex.EncodeToString(hash[:])
		if got := resp.Metadata["content-hash"]; got != want {
			t.Errorf("content-hash: got %q, want %q", got, want)
		}
	})

	t.Run("fetch by content hash", func(t *testing.T) {
		// Both backends index the body hash on write; no rebuild needed.
		// First fetch to get the content-hash
		stream := newMockStream("FETCH /hello.md\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		contentHash := resp.Metadata["content-hash"]

		// Now fetch by hash
		stream2 := newMockStream("FETCH /" + contentHash + "\n")
		h.HandleStream(stream2)
		resp2, err := protocol.ParseResponse(&stream2.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp2.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp2.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp2.Body, "# Hello World") {
			t.Errorf("body missing content: %q", resp2.Body)
		}
	})

	t.Run("fetch by unknown hash", func(t *testing.T) {
		stream := newMockStream("FETCH /sha256-0000000000000000000000000000000000000000000000000000000000000000\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("flat file not served", func(t *testing.T) {
		// File-only: a raw file without version history is a file-store fixture.
		flatDir := setupContentDir(t, map[string]string{
			"flat.md": "# Flat\n",
		})
		flatH := &Handler{Store: store.New(flatDir), Logger: discardLogger}

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

func TestHealthCheck(t *testing.T) { forEachBackend(t, testHealthCheck) }

func testHealthCheck(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"hello.md": "# Hello\n",
	})
	h := newHandler(b, nil)

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

func TestEtagInResponse(t *testing.T) { forEachBackend(t, testEtagInResponse) }

func testEtagInResponse(t *testing.T, newBackend backendFactory) {
	body := []byte("# Hello World\n")
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"hello.md": string(body),
	})
	h := newHandler(b, nil)

	stream := newMockStream("FETCH /hello.md\n")
	h.HandleStream(stream)

	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	stored, err := store.SerializeVersion(1, nil, body, nil)
	if err != nil {
		t.Fatalf("serialize expected stored bytes: %v", err)
	}
	want := store.StoredETag(stored)
	if resp.Metadata["etag"] != want {
		t.Errorf("etag = %q, want stored-byte hash %q", resp.Metadata["etag"], want)
	}
}

func TestConditionalFetch(t *testing.T) { forEachBackend(t, testConditionalFetch) }

func testConditionalFetch(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"hello.md": "# Hello World\n",
	})
	h := newHandler(b, nil)

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

func TestConditionalFetchMetadataChange(t *testing.T) {
	forEachBackend(t, testConditionalFetchMetadataChange)
}

func testConditionalFetchMetadataChange(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	body := []byte("# Same Body\n")
	first, err := b.Store.WriteVersion("/doc.md", 0, body, map[string]string{"tags": "first"})
	if err != nil {
		t.Fatalf("write v1: %v", err)
	}
	second, err := b.Store.WriteVersion("/doc.md", 1, body, map[string]string{"tags": "second"})
	if err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if first.ETag == second.ETag {
		t.Fatal("metadata-only update did not rotate stored-byte ETag")
	}

	h := newHandler(b, nil)
	stream := newMockStream("FETCH /doc.md\n---\nif-none-match: " + first.ETag + "\n---\n")
	h.HandleStream(stream)
	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Status != protocol.StatusOK || resp.Metadata["etag"] != second.ETag || resp.Metadata["version"] != "2" {
		t.Errorf("metadata update response status=%q etag=%q version=%q, want ok/%q/2",
			resp.Status, resp.Metadata["etag"], resp.Metadata["version"], second.ETag)
	}
	if resp.Metadata["content-hash"] != store.ContentHash(body) {
		t.Errorf("content hash = %q, want unchanged %q", resp.Metadata["content-hash"], store.ContentHash(body))
	}
}

func TestSymlinkEscape(t *testing.T) {
	// Create a file outside the content directory.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("SECRET DATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	// File-only: plants a symlink inside the content root.
	dir := t.TempDir()
	b := fileBackendAt(dir)
	seedBackend(t, b, map[string]string{
		"public.md": "# Public\n",
	})
	symlinkPath := filepath.Join(dir, "evil.md")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	h := newHandler(b, nil)

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

func TestHandleList(t *testing.T) { forEachBackend(t, testHandleList) }

func testHandleList(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	for _, f := range []struct{ path, content string }{
		{"/index.md", "# Index\n"},
		{"/about.md", "# About\n"},
		{"/docs/guide.md", "# Guide\n"},
		{"/docs/reference.md", "# Reference\n"},
	} {
		mustWrite(t, b, f.path, []byte(f.content), nil)
	}
	mustWrite(t, b, "/.hidden.md", []byte("secret\n"), nil)
	h := newHandler(b, nil)

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

func TestFetchDirectory(t *testing.T) { forEachBackend(t, testFetchDirectory) }

func testFetchDirectory(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)

	// Create files: docs/ has an index.md, api/ does not
	for _, f := range []struct{ path, content string }{
		{"/docs/index.md", "# Docs Home\n"},
		{"/docs/guide.md", "# Guide\n"},
		{"/api/users.md", "# Users API\n"},
		{"/api/auth.md", "# Auth API\n"},
	} {
		mustWrite(t, b, f.path, []byte(f.content), nil)
	}
	h := newHandler(b, nil)

	t.Run("directory with index.md serves document", func(t *testing.T) {
		stream := newMockStream("FETCH /docs/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "# Docs Home") {
			t.Errorf("body should contain index.md content, got %q", resp.Body)
		}
		if resp.Metadata["version"] == "" {
			t.Error("expected version metadata for index.md")
		}
		if resp.Metadata["etag"] == "" {
			t.Error("expected etag metadata for index.md")
		}
	})

	t.Run("directory with archived index.md falls back to listing", func(t *testing.T) {
		if _, _, err := b.Store.Archive("/docs/index.md", true); err != nil {
			t.Fatalf("archive index.md: %v", err)
		}
		stream := newMockStream("FETCH /docs/\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		// An archived index.md must not tombstone the whole directory: the
		// generated listing of live entries is served instead, and the hidden
		// index.md stays out of it.
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "# Index of /docs/") {
			t.Errorf("body should contain generated index header, got %q", resp.Body)
		}
		if !strings.Contains(resp.Body, "[guide.md]") {
			t.Error("body should list live guide.md")
		}
		if strings.Contains(resp.Body, "[index.md]") {
			t.Error("body should not list the archived index.md")
		}
		// The archived index.md itself still fetches as a tombstone.
		docStream := newMockStream("FETCH /docs/index.md\n")
		h.HandleStream(docStream)
		docResp, err := protocol.ParseResponse(&docStream.output)
		if err != nil {
			t.Fatalf("parse doc response: %v", err)
		}
		if docResp.Status != protocol.StatusArchived {
			t.Errorf("doc status: got %q, want %q", docResp.Status, protocol.StatusArchived)
		}
		// Unarchive to avoid affecting subsequent tests
		if _, _, err := b.Store.Archive("/docs/index.md", false); err != nil {
			t.Fatalf("unarchive index.md: %v", err)
		}
	})

	t.Run("directory without index.md generates listing", func(t *testing.T) {
		stream := newMockStream("FETCH /api/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "# Index of /api/") {
			t.Errorf("body should contain index header, got %q", resp.Body)
		}
		if !strings.Contains(resp.Body, "[users.md]") {
			t.Error("body should list users.md")
		}
		if !strings.Contains(resp.Body, "[auth.md]") {
			t.Error("body should list auth.md")
		}
		if resp.Metadata["entries"] == "" {
			t.Error("expected entries metadata for generated listing")
		}
		if resp.Metadata["version"] != "" {
			t.Error("generated listing should not have version metadata")
		}
	})

	t.Run("directory without trailing slash generates listing", func(t *testing.T) {
		stream := newMockStream("FETCH /api\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "# Index of /api") {
			t.Errorf("body should contain index header, got %q", resp.Body)
		}
	})

	t.Run("nonexistent directory returns not-found", func(t *testing.T) {
		stream := newMockStream("FETCH /nope/\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("root directory generates listing", func(t *testing.T) {
		stream := newMockStream("FETCH /\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if !strings.Contains(resp.Body, "[docs/]") {
			t.Error("body should list docs/ directory")
		}
		if !strings.Contains(resp.Body, "[api/]") {
			t.Error("body should list api/ directory")
		}
	})
}

func TestMultipleLeadingSlashes(t *testing.T) { forEachBackend(t, testMultipleLeadingSlashes) }

func testMultipleLeadingSlashes(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	mustWrite(t, b, "/hello.md", []byte("# Hello\n"), nil)
	mustWrite(t, b, "/docs/guide.md", []byte("# Guide\n"), nil)
	h := newHandler(b, nil)

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

func TestDeeplyNestedTraversal(t *testing.T) { forEachBackend(t, testDeeplyNestedTraversal) }

func testDeeplyNestedTraversal(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"safe.md": "# Safe\n",
	})
	h := newHandler(b, nil)

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
	if _, err := s.Write("/page.md", []byte("# Page\n"), nil); err != nil {
		t.Fatalf("write page: %v", err)
	}
	if _, err := s.Write("/docs/guide.md", []byte("# Guide\n"), nil); err != nil {
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
	h := &Handler{Store: relStore, Logger: discardLogger}

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
	if _, err := s.Write("/file.md", []byte("# Content\n"), nil); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := s.Write("/docs/guide.md", []byte("# Guide\n"), nil); err != nil {
		t.Fatalf("write guide: %v", err)
	}

	// Symlink another path to it.
	symlinkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(actualDir, symlinkDir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	symlinkStore := store.New(symlinkDir)
	h := &Handler{Store: symlinkStore, Logger: discardLogger}

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

func TestHandleVersions(t *testing.T) { forEachBackend(t, testHandleVersions) }

func testHandleVersions(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"doc.md": "# V1\n",
	})
	// Write a second version.
	mustWrite(t, b, "/doc.md", []byte("# V2\n"), nil)

	h := newHandler(b, nil)

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
		// File-only: raw flat file fixture.
		flatH := &Handler{Store: store.New(flatDir), Logger: discardLogger}

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
		noStoreH := &Handler{Logger: discardLogger}

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

func TestFetchVersion(t *testing.T) { forEachBackend(t, testFetchVersion) }

func testFetchVersion(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"doc.md": "# Version One\n",
	})
	mustWrite(t, b, "/doc.md", []byte("# Version Two\n"), nil)

	h := newHandler(b, nil)
	first, err := b.Store.Get("/doc.md", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
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
		if resp.Metadata["etag"] != first.ETag {
			t.Errorf("etag: got %q, want %q", resp.Metadata["etag"], first.ETag)
		}
	})

	t.Run("conditional fetch specific version", func(t *testing.T) {
		stream := newMockStream("FETCH /doc.md/v1\n---\nif-none-match: " + first.ETag + "\n---\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotModified || resp.Body != "" {
			t.Errorf("conditional historical fetch = status %q body %q, want not-modified empty", resp.Status, resp.Body)
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

func TestVersionsChainValid(t *testing.T) { forEachBackend(t, testVersionsChainValid) }

func TestVersionsChainOperationalFailure(t *testing.T) {
	b := fileBackend(t)
	seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
	b.Store = &verifyErrorStore{DocumentStore: b.Store, err: errors.New("backend unavailable")}
	b.Views = nil
	h := newHandler(b, nil)
	stream := newMockStream("VERSIONS /doc.md\n")
	h.HandleStream(stream)
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if response.Status != protocol.StatusServerError {
		t.Errorf("status = %q, want %q", response.Status, protocol.StatusServerError)
	}
}

type verifyErrorStore struct {
	DocumentStore
	err error
}

func (backend *verifyErrorStore) VerifyChain(string) error { return backend.err }

func testVersionsChainValid(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)

	// Write versions through the store to get proper hash chain.
	mustWrite(t, b, "/doc.md", []byte("# V1\n"), nil)
	mustWrite(t, b, "/doc.md", []byte("# V2\n"), nil)

	h := newHandler(b, nil)

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
		b.Tamper(t, "/doc.md", 1, []byte("# TAMPERED\n"))

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

func TestHandlePublish(t *testing.T) { forEachBackend(t, testHandlePublish) }

func testHandlePublish(t *testing.T, newBackend backendFactory) {
	// A permissive token store for tests that need to exercise write logic.
	const testSecret = "test-publish-secret"
	publishTokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(testSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})
	authMeta := "---\nauth: " + testSecret + "\n---\n"

	t.Run("creates new document", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, publishTokenStore)

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
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Original\n"), nil)
		h := newHandler(b, publishTokenStore)

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
		h := &Handler{Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return publishTokenStore }}

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

	t.Run("duplicate content is no-op", func(t *testing.T) {
		b := newBackend(t)
		// v1 carries the type the handler defaults to, so the republish below is
		// a true content+metadata duplicate (not a metadata change).
		mustWrite(t, b, "/doc.md", []byte("# Same\n"), map[string]string{"type": protocol.OKFDefaultType})
		h := newHandler(b, publishTokenStore)

		stream := newMockStream("PUBLISH /doc.md\n" + authMeta + "# Same\n")
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

	t.Run("path traversal blocked", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, publishTokenStore)

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

	t.Run("conflict on stale expected-version", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# v1\n"), nil)
		mustWrite(t, b, "/doc.md", []byte("# v2\n"), nil)
		h := newHandler(b, publishTokenStore)

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\nexpected-version: \"1\"\n---\n# stale edit\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusConflict {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusConflict)
		}
		if resp.Metadata["server-version"] != "2" {
			t.Errorf("server-version: got %q, want %q", resp.Metadata["server-version"], "2")
		}
	})

	t.Run("matching expected-version succeeds", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# v1\n"), nil)
		h := newHandler(b, publishTokenStore)

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\nexpected-version: \"1\"\n---\n# v2\n")
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

	t.Run("no expected-version is backward compatible", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# v1\n"), nil)
		h := newHandler(b, publishTokenStore)

		stream := newMockStream("PUBLISH /doc.md\n" + authMeta + "# v2 no check\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
	})

	t.Run("invalid expected-version returns bad-request", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, publishTokenStore)

		for _, ev := range []string{"abc", "-1", "1.5"} {
			stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\nexpected-version: \"" + ev + "\"\n---\n# content\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response for %q: %v", ev, err)
			}
			if resp.Status != protocol.StatusBadRequest {
				t.Errorf("expected-version=%q: status got %q, want %q", ev, resp.Status, protocol.StatusBadRequest)
			}
		}
	})
}

func TestHandleWrite_RejectsNonMarkdown(t *testing.T) {
	forEachBackend(t, testHandleWrite_RejectsNonMarkdown)
}

func testHandleWrite_RejectsNonMarkdown(t *testing.T, newBackend backendFactory) {
	const secret = "reject-secret"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {Paths: []string{"/*"}, Operations: []string{"publish"}},
	})
	authMeta := "---\nauth: " + secret + "\n---\n"
	// PNG magic: invalid UTF-8, so it fails the content check even at a .md path.
	png := "\x89PNG\r\n\x1a\n\xb1\xa5"

	tests := []struct {
		name string
		req  string
	}{
		{"publish png to .png path", "PUBLISH /img.png\n" + authMeta + png},
		{"publish png bytes to .md path", "PUBLISH /sneaky.md\n" + authMeta + png},
		{"publish text to .txt path", "PUBLISH /notes.txt\n" + authMeta + "plain text\n"},
		// Empty body must not slip past the .md contract via the no-op shortcut.
		{"publish empty body to .txt path", "PUBLISH /notes.txt\n" + authMeta},
		{"append needs expected-version too, but path is gated first",
			"APPEND /img.png\n---\nauth: " + secret + "\nexpected-version: 1\n---\nmore\n"},
		{"append empty body to .txt path",
			"APPEND /notes.txt\n---\nauth: " + secret + "\nexpected-version: 1\n---\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBackend(t)
			h := newHandler(b, ts)

			stream := newMockStream(tt.req)
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != protocol.StatusBadRequest {
				t.Errorf("status: got %q, want %q (body: %q)", resp.Status, protocol.StatusBadRequest, resp.Body)
			}
		})
	}

	t.Run("valid markdown still publishes", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, ts)
		stream := newMockStream("PUBLISH /ok.md\n" + authMeta + "# Good\n")
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

func TestHandlePublishAuth(t *testing.T) { forEachBackend(t, testHandlePublishAuth) }

func testHandlePublishAuth(t *testing.T, newBackend backendFactory) {
	// Raw secrets used in requests. Store keys are their hashes.
	const (
		writerSecret   = "writer-secret"
		readonlySecret = "readonly-secret"
	)

	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(writerSecret): {
			Paths:      []string{"/docs/*"},
			Operations: []string{"publish"},
		},
		protocol.HashToken(readonlySecret): {
			Paths:      []string{"/*"},
			Operations: []string{"read"},
		},
	})

	t.Run("no token store denies publishing", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, nil)

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
		b := newBackend(t)
		h := newHandler(b, ts)

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
		b := newBackend(t)
		h := newHandler(b, ts)

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
		b := newBackend(t)
		h := newHandler(b, ts)

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
		b := newBackend(t)
		h := newHandler(b, ts)

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
		b := newBackend(t)
		h := newHandler(b, ts)

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

func TestIsHashPath(t *testing.T) {
	tests := []struct {
		path string
		hash string
		ok   bool
	}{
		{"/sha256-" + strings.Repeat("ab", 32), "sha256-" + strings.Repeat("ab", 32), true},
		{"/sha256-0000000000000000000000000000000000000000000000000000000000000000", "sha256-0000000000000000000000000000000000000000000000000000000000000000", true},
		{"/sha256-short", "", false},
		{"/doc.md", "", false},
		{"/sha256-" + strings.Repeat("GG", 32), "", false}, // uppercase not valid
		{"/sha256-" + strings.Repeat("ab", 32) + "extra", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			hash, ok := protocol.IsHashPath(tt.path)
			if ok != tt.ok || hash != tt.hash {
				t.Errorf("IsHashPath(%q) = (%q, %v), want (%q, %v)",
					tt.path, hash, ok, tt.hash, tt.ok)
			}
		})
	}
}

func TestHandleArchive(t *testing.T) { forEachBackend(t, testHandleArchive) }

func TestArchiveUsesCommittedResult(t *testing.T) {
	const secret = "archive-result-secret"
	b := fileBackend(t)
	spy := &archiveResultStore{
		DocumentStore: b.Store,
		document:      &store.Document{Version: 2, Archived: true},
		changed:       true,
	}
	b.Store = spy
	h := newHandler(b, auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {Paths: []string{"/*"}, Operations: []string{"publish"}},
	}))
	stream := newMockStream("ARCHIVE /doc.md\n---\nauth: " + secret + "\n---\n")
	h.HandleStream(stream)
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if response.Status != protocol.StatusOK || response.Metadata["version"] != "2" {
		t.Errorf("archive response = status %q version %q", response.Status, response.Metadata["version"])
	}
	if spy.getCalls != 0 || spy.archiveCalls != 1 {
		t.Errorf("store calls = Get %d Archive %d, want 0/1", spy.getCalls, spy.archiveCalls)
	}
}

type archiveResultStore struct {
	DocumentStore
	document     *store.Document
	changed      bool
	err          error
	getCalls     int
	archiveCalls int
}

func (spy *archiveResultStore) Get(path string, version int) (*store.Document, error) {
	spy.getCalls++
	return spy.DocumentStore.Get(path, version)
}

func (spy *archiveResultStore) Archive(string, bool) (*store.Document, bool, error) {
	spy.archiveCalls++
	return spy.document, spy.changed, spy.err
}

func testHandleArchive(t *testing.T, newBackend backendFactory) {
	writerSecret := "test-secret-key"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(writerSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})

	t.Run("archive not found", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, ts)

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
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)

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
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)

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
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)

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
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)

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
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)

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
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)
		first, err := b.Store.Get("/doc.md", 1)
		if err != nil {
			t.Fatalf("get v1: %v", err)
		}

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
		if resp.Metadata["etag"] != first.ETag || resp.Body != "# Content\n" {
			t.Errorf("archived pinned fetch = etag %q body %q, want %q and original body", resp.Metadata["etag"], resp.Body, first.ETag)
		}

		stream = newMockStream("FETCH /doc.md/v1\n---\nif-none-match: " + first.ETag + "\n---\n")
		h.HandleStream(stream)
		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse conditional response: %v", err)
		}
		if resp.Status != protocol.StatusNotModified || resp.Body != "" {
			t.Errorf("archived conditional pinned fetch = status %q body %q, want not-modified empty", resp.Status, resp.Body)
		}
	})

	t.Run("publish with body to active document still works", func(t *testing.T) {
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)

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
		b := newBackend(t)
		seedBackend(t, b, map[string]string{"doc.md": "# Content\n"})
		h := newHandler(b, ts)

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

func TestHandleAppend(t *testing.T) { forEachBackend(t, testHandleAppend) }

func testHandleAppend(t *testing.T, newBackend backendFactory) {
	const testSecret = "test-append-secret"
	appendTokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(testSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})
	authMetaV1 := "---\nauth: " + testSecret + "\nexpected-version: \"1\"\n---\n"

	t.Run("appends to existing document", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Start"), nil)
		h := newHandler(b, appendTokenStore)

		stream := newMockStream("APPEND /doc.md\n" + authMetaV1 + "More text.")
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

	t.Run("not found when document does not exist", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, appendTokenStore)

		stream := newMockStream("APPEND /missing.md\n" + authMetaV1 + "content")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusNotFound {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotFound)
		}
	})

	t.Run("rejects empty body", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Existing"), nil)
		h := newHandler(b, appendTokenStore)

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\nexpected-version: \"1\"\n---\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusServerError {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusServerError)
		}
	})

	t.Run("requires expected-version", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Existing"), nil)
		h := newHandler(b, appendTokenStore)

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\n---\nMore text.")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusBadRequest {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusBadRequest)
		}
	})

	t.Run("auth required", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Existing"), nil)
		h := newHandler(b, appendTokenStore)

		stream := newMockStream("APPEND /doc.md\n---\nexpected-version: \"1\"\n---\nMore text.")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusUnauthorized {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusUnauthorized)
		}
	})

	t.Run("conflict on stale expected-version", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# V1"), nil)
		mustWrite(t, b, "/doc.md", []byte("# V2"), nil)
		h := newHandler(b, appendTokenStore)

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\nexpected-version: \"1\"\n---\nLate append.")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusConflict {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusConflict)
		}
	})

	t.Run("archived document rejected", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Content"), nil)
		if _, _, err := b.Store.Archive("/doc.md", true); err != nil {
			t.Fatal(err)
		}
		h := newHandler(b, appendTokenStore)

		stream := newMockStream("APPEND /doc.md\n" + authMetaV1 + "More.")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusArchived {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusArchived)
		}
	})

	t.Run("combined content exceeds size limit", func(t *testing.T) {
		b := newBackend(t)
		initial := make([]byte, protocol.MaxBodyLength-100)
		for i := range initial {
			initial[i] = 'x'
		}
		mustWrite(t, b, "/doc.md", initial, nil)
		h := newHandler(b, appendTokenStore)

		appendBody := strings.Repeat("y", 200)
		stream := newMockStream("APPEND /doc.md\n" + authMetaV1 + appendBody)
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

func TestPublisherMetadata(t *testing.T) { forEachBackend(t, testPublisherMetadata) }

func testPublisherMetadata(t *testing.T, newBackend backendFactory) {
	const testSecret = "test-meta-secret"
	tokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(testSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})

	t.Run("publish with metadata and fetch it back", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		// Publish with publisher metadata.
		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\ntype: journal\nauthor: claude\n---\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse publish response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("publish status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}

		// Fetch and verify metadata appears in response.
		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)

		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse fetch response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Fatalf("fetch status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if resp.Metadata["type"] != "journal" {
			t.Errorf("type: got %q, want %q", resp.Metadata["type"], "journal")
		}
		if resp.Metadata["author"] != "claude" {
			t.Errorf("author: got %q, want %q", resp.Metadata["author"], "claude")
		}
	})

	t.Run("control keys not stored", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\nexpected-version: 0\ntype: note\n---\n# Hello\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}

		// Fetch back — auth and expected-version should not be in response.
		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)

		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse fetch response: %v", err)
		}
		if _, ok := resp.Metadata["auth"]; ok {
			t.Error("auth should not be in response metadata")
		}
		if _, ok := resp.Metadata["expected-version"]; ok {
			t.Error("expected-version should not be in response metadata")
		}
		if resp.Metadata["type"] != "note" {
			t.Errorf("type: got %q, want %q", resp.Metadata["type"], "note")
		}
	})

	t.Run("too many metadata keys", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		// Build frontmatter with one more than MaxMetaKeys non-control keys,
		// using short keys/values so the key-count limit trips before the
		// byte limit.
		var fm strings.Builder
		fm.WriteString("---\nauth: " + testSecret + "\n")
		for i := range protocol.MaxMetaKeys + 1 {
			fm.WriteString("k" + strconv.Itoa(i) + ": v\n")
		}
		fm.WriteString("---\n# Content\n")

		stream := newMockStream("PUBLISH /doc.md\n" + fm.String())
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusBadRequest {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusBadRequest)
		}
	})

	t.Run("reserved metadata keys rejected", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		for _, key := range []string{"version", "modified", "etag", "current-version", "server-version", "matches"} {
			stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\n" + key + ": evil\n---\n# Content\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response for key %q: %v", key, err)
			}
			if resp.Status != protocol.StatusBadRequest {
				t.Errorf("key %q: got status %q, want %q", key, resp.Status, protocol.StatusBadRequest)
			}
		}
	})

	t.Run("invalid metadata key characters rejected", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		for _, key := range []string{"UPPER", "under_score", "dot.key", "slash/key"} {
			stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\n" + key + ": val\n---\n# Content\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response for key %q: %v", key, err)
			}
			if resp.Status != protocol.StatusBadRequest {
				t.Errorf("key %q: got status %q, want %q", key, resp.Status, protocol.StatusBadRequest)
			}
		}
	})

	t.Run("append with metadata", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Start"), map[string]string{"type": "note"})
		h := newHandler(b, tokenStore)

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\nexpected-version: 1\ntype: journal\n---\nMore content.\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}

		// Fetch current version — should have the append's metadata.
		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)

		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse fetch response: %v", err)
		}
		if resp.Metadata["type"] != "journal" {
			t.Errorf("type: got %q, want %q", resp.Metadata["type"], "journal")
		}
	})

	t.Run("legacy document without metadata", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Hello\n"), nil)
		h := newHandler(b, tokenStore)

		stream := newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		// Should have standard metadata but no publisher metadata.
		if resp.Metadata["version"] != "1" {
			t.Errorf("version: got %q, want %q", resp.Metadata["version"], "1")
		}
		// No extra keys beyond standard metadata: version, modified, etag, content-hash.
		for k := range resp.Metadata {
			switch k {
			case "version", "modified", "etag", "content-hash":
				// expected
			default:
				t.Errorf("unexpected metadata key %q in legacy document", k)
			}
		}
	})

	t.Run("okf type default applied when absent", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\ntitle: Untyped\n---\n# Content\n")
		h.HandleStream(stream)

		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse fetch: %v", err)
		}
		if resp.Metadata["type"] != protocol.OKFDefaultType {
			t.Errorf("type: got %q, want default %q", resp.Metadata["type"], protocol.OKFDefaultType)
		}
	})

	t.Run("okf type default not applied to reserved index.md", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		stream := newMockStream("PUBLISH /index.md\n---\nauth: " + testSecret + "\n---\n# Hub\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse publish: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("publish status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}

		stream = newMockStream("FETCH /index.md\n")
		h.HandleStream(stream)
		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse fetch: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Fatalf("fetch status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if _, ok := resp.Metadata["type"]; ok {
			t.Errorf("reserved index.md should not get a default type, got %q", resp.Metadata["type"])
		}
	})

	t.Run("explicit type preserved", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\ntype: Metric\n---\n# Content\n")
		h.HandleStream(stream)

		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse fetch: %v", err)
		}
		if resp.Metadata["type"] != "Metric" {
			t.Errorf("explicit type: got %q, want %q", resp.Metadata["type"], "Metric")
		}
	})

	t.Run("okf type default applied on append", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# Start\n"), nil)
		h := newHandler(b, tokenStore)

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\nexpected-version: 1\n---\nMore.\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse append: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("append status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}

		stream = newMockStream("FETCH /doc.md\n")
		h.HandleStream(stream)
		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse fetch: %v", err)
		}
		if resp.Metadata["type"] != protocol.OKFDefaultType {
			t.Errorf("append type default: got %q, want %q", resp.Metadata["type"], protocol.OKFDefaultType)
		}
	})

	t.Run("type default pushing over cap returns bad-request", func(t *testing.T) {
		b := newBackend(t)
		h := newHandler(b, tokenStore)

		// Exactly MaxMetaKeys publisher keys and no type: the default type push
		// the count to MaxMetaKeys+1, which must be rejected as bad-request (not
		// fall through to a generic server error at write time).
		var fm strings.Builder
		fm.WriteString("---\nauth: " + testSecret + "\n")
		for i := range protocol.MaxMetaKeys {
			fm.WriteString("k" + strconv.Itoa(i) + ": v\n")
		}
		fm.WriteString("---\n# Content\n")

		stream := newMockStream("PUBLISH /doc.md\n" + fm.String())
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusBadRequest {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusBadRequest)
		}
	})

	t.Run("fetch version preserves metadata", func(t *testing.T) {
		b := newBackend(t)
		mustWrite(t, b, "/doc.md", []byte("# V1\n"), map[string]string{"type": "draft"})
		mustWrite(t, b, "/doc.md", []byte("# V2\n"), map[string]string{"type": "published"})
		h := newHandler(b, tokenStore)

		// Fetch v1 — should have its own metadata.
		stream := newMockStream("FETCH /doc.md/v1\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
		if resp.Metadata["type"] != "draft" {
			t.Errorf("v1 type: got %q, want %q", resp.Metadata["type"], "draft")
		}
	})
}

func TestReadAuth(t *testing.T) { forEachBackend(t, testReadAuth) }

func testReadAuth(t *testing.T, newBackend backendFactory) {
	const readSecret = "read-secret"

	tokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(readSecret): {
			Label:      "reader",
			Paths:      []string{"/private/**"},
			Operations: []string{"read"},
		},
	})

	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"private/secret.md": "# Secret\n",
		"public/open.md":    "# Open\n",
	})

	// Write the well-known manifest so we can test it's always accessible.
	mustWrite(t, b, "/.well-known/agent-manifest.md", []byte("# Manifest\n"), nil)

	h := &Handler{
		Store:         b.Store,
		Catalog:       b.Catalog,
		GetTokenStore: func() *auth.TokenStore { return tokenStore },
		Logger:        discardLogger,
	}

	tests := []struct {
		name       string
		request    string
		wantStatus string
	}{
		{
			"FETCH protected path without token",
			"FETCH /private/secret.md\n",
			protocol.StatusUnauthorized,
		},
		{
			"FETCH protected path with valid token",
			"FETCH /private/secret.md\n---\nauth: " + readSecret + "\n---\n",
			protocol.StatusOK,
		},
		{
			"FETCH protected path with wrong token",
			"FETCH /private/secret.md\n---\nauth: wrong-token\n---\n",
			protocol.StatusUnauthorized,
		},
		{
			"FETCH protected path with duplicate slash alias",
			"FETCH //private/secret.md\n",
			protocol.StatusUnauthorized,
		},
		{
			"FETCH protected path with dot alias",
			"FETCH /private/./secret.md\n",
			protocol.StatusUnauthorized,
		},
		{
			"FETCH public path without token",
			"FETCH /public/open.md\n",
			protocol.StatusOK,
		},
		{
			"LIST protected path without token",
			"LIST /private/\n",
			protocol.StatusUnauthorized,
		},
		{
			"LIST protected path with valid token",
			"LIST /private/\n---\nauth: " + readSecret + "\n---\n",
			protocol.StatusOK,
		},
		{
			"FETCH protected versioned path without token",
			"FETCH /private/secret.md/v1\n",
			protocol.StatusUnauthorized,
		},
		{
			"FETCH protected versioned path with valid token",
			"FETCH /private/secret.md/v1\n---\nauth: " + readSecret + "\n---\n",
			protocol.StatusOK,
		},
		{
			"VERSIONS protected path without token",
			"VERSIONS /private/secret.md\n",
			protocol.StatusUnauthorized,
		},
		{
			"VERSIONS protected path with valid token",
			"VERSIONS /private/secret.md\n---\nauth: " + readSecret + "\n---\n",
			protocol.StatusOK,
		},
		{
			"FETCH well-known manifest without token on protected server",
			"FETCH /.well-known/agent-manifest.md\n",
			protocol.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := newMockStream(tt.request)
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != tt.wantStatus {
				t.Errorf("status: got %q, want %q", resp.Status, tt.wantStatus)
			}
		})
	}

	t.Run("no token store means all reads public", func(t *testing.T) {
		noAuth := newHandler(b, nil)
		stream := newMockStream("FETCH /private/secret.md\n")
		noAuth.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("FETCH by hash protected path without token", func(t *testing.T) {
		// First fetch the doc with auth to get its content hash.
		stream := newMockStream("FETCH /private/secret.md\n---\nauth: " + readSecret + "\n---\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		hash := resp.Metadata["content-hash"]
		if hash == "" {
			t.Fatal("no content-hash in response")
		}

		// Now try fetching by hash without auth — should be denied.
		stream = newMockStream("FETCH /" + hash + "\n")
		h.HandleStream(stream)
		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusUnauthorized {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusUnauthorized)
		}
	})

	t.Run("FETCH by hash protected path with valid token", func(t *testing.T) {
		stream := newMockStream("FETCH /private/secret.md\n---\nauth: " + readSecret + "\n---\n")
		h.HandleStream(stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		hash := resp.Metadata["content-hash"]

		stream = newMockStream("FETCH /" + hash + "\n---\nauth: " + readSecret + "\n---\n")
		h.HandleStream(stream)
		resp, err = protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})
}

func TestDirectoryReadAuthFiltering(t *testing.T) {
	forEachBackend(t, testDirectoryReadAuthFiltering)
}

func testDirectoryReadAuthFiltering(t *testing.T, newBackend backendFactory) {
	const readSecret = "directory-read-secret"
	tokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(readSecret): {
			Paths:      []string{"/mixed/secret.md", "/docs/index.md"},
			Operations: []string{"read"},
		},
	})
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"mixed/public.md": "# Public\n",
		"mixed/secret.md": "# Secret\n",
		"docs/index.md":   "# Protected Index\n",
	})
	h := newHandler(b, tokenStore)

	tests := []struct {
		name        string
		request     string
		wantStatus  string
		wantEntries string
		wantBody    string
		hideBody    string
	}{
		{"LIST filters protected child", "LIST /mixed/\n", protocol.StatusOK, "1", "public.md", "secret.md"},
		{"LIST token reveals protected child", "LIST /mixed/\n---\nauth: " + readSecret + "\n---\n", protocol.StatusOK, "2", "secret.md", ""},
		{"generated index filters protected child", "FETCH /mixed/\n", protocol.StatusOK, "1", "public.md", "secret.md"},
		{"generated index token reveals protected child", "FETCH /mixed/\n---\nauth: " + readSecret + "\n---\n", protocol.StatusOK, "2", "secret.md", ""},
		{"explicit index requires its own auth", "FETCH /docs/\n", protocol.StatusUnauthorized, "", "", "Protected Index"},
		{"explicit index accepts token", "FETCH /docs/\n---\nauth: " + readSecret + "\n---\n", protocol.StatusOK, "", "Protected Index", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := newMockStream(test.request)
			h.HandleStream(stream)
			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", resp.Status, test.wantStatus)
			}
			if test.wantEntries != "" && resp.Metadata["entries"] != test.wantEntries {
				t.Errorf("entries = %q, want %q", resp.Metadata["entries"], test.wantEntries)
			}
			if strings.HasPrefix(test.request, "FETCH /mixed/") && resp.Status == protocol.StatusOK {
				if resp.Metadata["etag"] == "" || resp.Metadata["content-hash"] == "" {
					t.Errorf("generated index lacks cache metadata: %v", resp.Metadata)
				}
			}
			if test.wantBody != "" && !strings.Contains(resp.Body, test.wantBody) {
				t.Errorf("body missing %q: %s", test.wantBody, resp.Body)
			}
			if test.hideBody != "" && strings.Contains(resp.Body, test.hideBody) {
				t.Errorf("body leaked %q: %s", test.hideBody, resp.Body)
			}
		})
	}

	t.Run("generated index supports conditional fetch", func(t *testing.T) {
		stream := newMockStream("FETCH /mixed/\n")
		h.HandleStream(stream)
		first, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse initial response: %v", err)
		}
		stream = newMockStream("FETCH /mixed/\n---\nif-none-match: " + first.Metadata["etag"] + "\n---\n")
		h.HandleStream(stream)
		conditional, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse conditional response: %v", err)
		}
		if conditional.Status != protocol.StatusNotModified || conditional.Body != "" {
			t.Errorf("conditional generated index = status %q body %q", conditional.Status, conditional.Body)
		}
	})
}

type hashResultStore struct {
	DocumentStore
	path string
	err  error
}

func (s *hashResultStore) LookupHash(string) (string, error) {
	return s.path, s.err
}

func TestFetchHashLookupFailure(t *testing.T) {
	forEachBackend(t, testFetchHashLookupFailure)
}

func testFetchHashLookupFailure(t *testing.T, newBackend backendFactory) {
	hashPath := "/sha256-0000000000000000000000000000000000000000000000000000000000000000"
	tests := []struct {
		name string
		path string
		err  error
		want string
	}{
		{"backend error", "", errors.New("storage unavailable"), protocol.StatusServerError},
		{"resolved path disappeared", "/missing.md", nil, protocol.StatusServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := newBackend(t)
			h := newHandler(b, nil)
			h.Store = &hashResultStore{DocumentStore: b.Store, path: test.path, err: test.err}
			h.Views = nil
			stream := newMockStream("FETCH " + hashPath + "\n")
			h.HandleStream(stream)
			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != test.want {
				t.Errorf("status = %q, want %q", resp.Status, test.want)
			}
		})
	}
}

func TestReadOnlyMode(t *testing.T) { forEachBackend(t, testReadOnlyMode) }

func testReadOnlyMode(t *testing.T, newBackend backendFactory) {
	b := newBackend(t)
	seedBackend(t, b, map[string]string{
		"doc.md": "# Existing\n",
	})
	h := &Handler{
		Store:    b.Store,
		Catalog:  b.Catalog,
		Logger:   discardLogger,
		ReadOnly: true,
		GetTokenStore: func() *auth.TokenStore {
			return auth.NewTokenStore(map[string]auth.Token{
				protocol.HashToken("secret"): {Paths: []string{"/*"}, Operations: []string{"publish", "append", "archive"}},
			})
		},
	}
	authMeta := "---\nauth: secret\n---\n"

	for _, verb := range []string{"PUBLISH", "APPEND", "ARCHIVE"} {
		t.Run(verb+" rejected", func(t *testing.T) {
			stream := newMockStream(verb + " /doc.md\n" + authMeta + "# Content\n")
			h.HandleStream(stream)

			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != protocol.StatusNotPermitted {
				t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusNotPermitted)
			}
			if !strings.Contains(resp.Body, "read-only") {
				t.Errorf("body should mention read-only, got %q", resp.Body)
			}
		})
	}

	t.Run("FETCH still works", func(t *testing.T) {
		stream := newMockStream("FETCH /doc.md\n\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("LIST still works", func(t *testing.T) {
		stream := newMockStream("LIST /\n\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusOK {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusOK)
		}
	})

	t.Run("VERSIONS still works", func(t *testing.T) {
		stream := newMockStream("VERSIONS /doc.md\n\n")
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

func TestHandlePublish_Retention(t *testing.T) { forEachBackend(t, testHandlePublish_Retention) }

func testHandlePublish_Retention(t *testing.T, newBackend backendFactory) {
	const testSecret = "test-retention-secret"
	tokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(testSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})

	t.Run("prunes and audit-logs", func(t *testing.T) {
		b := newBackend(t)
		for i := range 5 {
			mustWrite(t, b, "/doc.md", []byte("# Version "+strconv.Itoa(i+1)), nil)
		}
		var logBuf bytes.Buffer
		h := &Handler{Store: b.Store, Catalog: b.Catalog, Logger: slog.New(slog.NewTextHandler(&logBuf, nil)), GetTokenStore: func() *auth.TokenStore { return tokenStore }}

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\nretention: 2\n---\n# Version 6\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
		versions, err := b.Store.Versions("/doc.md")
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		if len(versions) != 2 {
			t.Errorf("remaining versions = %d, want 2", len(versions))
		}
		log := logBuf.String()
		for _, want := range []string{"msg=prune", "operation=PUBLISH", "pruned_from=1", "pruned_to=4", "path=/doc.md", "success=true"} {
			if !strings.Contains(log, want) {
				t.Errorf("audit log missing %q:\n%s", want, log)
			}
		}
	})

	t.Run("invalid retention rejected", func(t *testing.T) {
		for _, value := range []string{"zero", "0", "-1"} {
			t.Run("retention="+value, func(t *testing.T) {
				b := newBackend(t)
				h := newHandler(b, tokenStore)

				stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\nretention: " + value + "\n---\n# Hello\n")
				h.HandleStream(stream)

				resp, err := protocol.ParseResponse(&stream.output)
				if err != nil {
					t.Fatalf("parse response: %v", err)
				}
				if resp.Status != protocol.StatusBadRequest {
					t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusBadRequest)
				}
			})
		}
	})

	t.Run("no prune without retention", func(t *testing.T) {
		b := newBackend(t)
		for i := range 3 {
			mustWrite(t, b, "/doc.md", []byte("# Version "+strconv.Itoa(i+1)), nil)
		}
		var logBuf bytes.Buffer
		h := &Handler{Store: b.Store, Catalog: b.Catalog, Logger: slog.New(slog.NewTextHandler(&logBuf, nil)), GetTokenStore: func() *auth.TokenStore { return tokenStore }}

		stream := newMockStream("PUBLISH /doc.md\n---\nauth: " + testSecret + "\n---\n# Version 4\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
		versions, err := b.Store.Versions("/doc.md")
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		if len(versions) != 4 {
			t.Errorf("remaining versions = %d, want 4", len(versions))
		}
		if strings.Contains(logBuf.String(), "msg=prune") {
			t.Errorf("unexpected prune audit entry:\n%s", logBuf.String())
		}
	})
}

func TestHandleAppend_Retention(t *testing.T) { forEachBackend(t, testHandleAppend_Retention) }

func testHandleAppend_Retention(t *testing.T, newBackend backendFactory) {
	const testSecret = "test-append-retention-secret"
	// APPEND authorizes under the "publish" operation (see handleAppend).
	tokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(testSecret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})

	seedDoc := func(t *testing.T, b backend, n int) {
		t.Helper()
		for i := range n {
			mustWrite(t, b, "/doc.md", []byte("# Version "+strconv.Itoa(i+1)), nil)
		}
	}

	t.Run("prunes and audit-logs", func(t *testing.T) {
		b := newBackend(t)
		seedDoc(t, b, 5)
		var logBuf bytes.Buffer
		h := &Handler{Store: b.Store, Catalog: b.Catalog, Logger: slog.New(slog.NewTextHandler(&logBuf, nil)), GetTokenStore: func() *auth.TokenStore { return tokenStore }}

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\nexpected-version: 5\nretention: 2\n---\nappended line\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
		versions, err := b.Store.Versions("/doc.md")
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		if len(versions) != 2 {
			t.Errorf("remaining versions = %d, want 2", len(versions))
		}
		log := logBuf.String()
		for _, want := range []string{"msg=prune", "operation=APPEND", "pruned_from=1", "pruned_to=4", "path=/doc.md", "success=true"} {
			if !strings.Contains(log, want) {
				t.Errorf("audit log missing %q:\n%s", want, log)
			}
		}
	})

	t.Run("invalid retention rejected", func(t *testing.T) {
		b := newBackend(t)
		seedDoc(t, b, 1)
		h := newHandler(b, tokenStore)

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\nexpected-version: 1\nretention: 0\n---\nappended line\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusBadRequest {
			t.Errorf("status: got %q, want %q", resp.Status, protocol.StatusBadRequest)
		}
		if versions, err := b.Store.Versions("/doc.md"); err != nil || len(versions) != 1 {
			t.Errorf("versions = %d (err %v), want 1 untouched", len(versions), err)
		}
	})

	t.Run("no prune without retention", func(t *testing.T) {
		b := newBackend(t)
		seedDoc(t, b, 3)
		var logBuf bytes.Buffer
		h := &Handler{Store: b.Store, Catalog: b.Catalog, Logger: slog.New(slog.NewTextHandler(&logBuf, nil)), GetTokenStore: func() *auth.TokenStore { return tokenStore }}

		stream := newMockStream("APPEND /doc.md\n---\nauth: " + testSecret + "\nexpected-version: 3\n---\nappended line\n")
		h.HandleStream(stream)

		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.Status != protocol.StatusCreated {
			t.Fatalf("status: got %q, want %q", resp.Status, protocol.StatusCreated)
		}
		if versions, err := b.Store.Versions("/doc.md"); err != nil || len(versions) != 4 {
			t.Errorf("versions = %d (err %v), want all 4", len(versions), err)
		}
		if strings.Contains(logBuf.String(), "msg=prune") {
			t.Errorf("unexpected prune audit entry:\n%s", logBuf.String())
		}
	})
}
