package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit/demarkus/protocol"
)

func TestPutAndGet(t *testing.T) {
	c := New(t.TempDir())

	resp := protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"modified": "2025-02-14T10:30:00Z",
			"version":  "1",
		},
		Body: "# Hello World\n",
	}

	if err := c.Put("localhost:6309", "/index.md", protocol.VerbFetch, resp); err != nil {
		t.Fatalf("put: %v", err)
	}

	entry, err := c.Get("localhost:6309", "/index.md", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if entry == nil {
		t.Fatal("expected cached entry, got nil")
	}
	if entry.Response.Status != protocol.StatusOK {
		t.Errorf("status: got %q, want %q", entry.Response.Status, protocol.StatusOK)
	}
	if entry.Response.Body != "# Hello World\n" {
		t.Errorf("body: got %q, want %q", entry.Response.Body, "# Hello World\n")
	}
	if entry.Response.Metadata["version"] != "1" {
		t.Errorf("version: got %q, want %q", entry.Response.Metadata["version"], "1")
	}
	if entry.CachedAt.IsZero() {
		t.Error("cached_at should not be zero")
	}
}

func TestCacheMiss(t *testing.T) {
	c := New(t.TempDir())

	entry, err := c.Get("localhost:6309", "/nonexistent.md", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if entry != nil {
		t.Error("expected nil for cache miss")
	}
}

func TestListAndFetchSeparate(t *testing.T) {
	c := New(t.TempDir())

	fetchResp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"version": "1"},
		Body:     "# Index\n",
	}
	listResp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"entries": "3"},
		Body:     "# Index of /\n\n- [a.md](a.md)\n",
	}

	if err := c.Put("localhost:6309", "/", protocol.VerbFetch, fetchResp); err != nil {
		t.Fatalf("put fetch: %v", err)
	}
	if err := c.Put("localhost:6309", "/", protocol.VerbList, listResp); err != nil {
		t.Fatalf("put list: %v", err)
	}

	fetchEntry, err := c.Get("localhost:6309", "/", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("get fetch: %v", err)
	}
	listEntry, err := c.Get("localhost:6309", "/", protocol.VerbList)
	if err != nil {
		t.Fatalf("get list: %v", err)
	}

	if fetchEntry.Response.Body == listEntry.Response.Body {
		t.Error("FETCH and LIST should be cached separately")
	}
	if fetchEntry.Response.Body != "# Index\n" {
		t.Errorf("fetch body: got %q", fetchEntry.Response.Body)
	}
	if listEntry.Response.Body != "# Index of /\n\n- [a.md](a.md)\n" {
		t.Errorf("list body: got %q", listEntry.Response.Body)
	}
}

func TestFetchThenListSamePath(t *testing.T) {
	c := New(t.TempDir())

	fetchResp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"version": "1"},
		Body:     "# Hello\n",
	}
	listResp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"total": "3"},
		Body:     "# Versions\n",
	}

	// FETCH first, then LIST the same .md path. Previously this failed
	// because FETCH created index.md as a file and LIST needed it as a dir.
	if err := c.Put("localhost:6309", "/index.md", protocol.VerbFetch, fetchResp); err != nil {
		t.Fatalf("put fetch: %v", err)
	}
	if err := c.Put("localhost:6309", "/index.md", protocol.VerbList, listResp); err != nil {
		t.Fatalf("put list: %v", err)
	}

	fetchEntry, _ := c.Get("localhost:6309", "/index.md", protocol.VerbFetch)
	listEntry, _ := c.Get("localhost:6309", "/index.md", protocol.VerbList)

	if fetchEntry == nil || listEntry == nil {
		t.Fatal("expected both entries to be cached")
	}
	if fetchEntry.Response.Body != "# Hello\n" {
		t.Errorf("fetch body: got %q", fetchEntry.Response.Body)
	}
	if listEntry.Response.Body != "# Versions\n" {
		t.Errorf("list body: got %q", listEntry.Response.Body)
	}
}

func TestStaleFlatFileCache(t *testing.T) {
	c := New(t.TempDir())

	// Simulate a stale flat-file cache entry from the old layout:
	// old layout stored FETCH /index.md as a regular file at host/index.md.
	hostDir := filepath.Join(c.Dir, "localhost:6309")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleFile := filepath.Join(hostDir, "index.md")
	if err := os.WriteFile(staleFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleFile+".meta", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Put with new layout should succeed despite the stale file.
	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"version": "1"},
		Body:     "# Hello\n",
	}
	if err := c.Put("localhost:6309", "/index.md", protocol.VerbFetch, resp); err != nil {
		t.Fatalf("put: %v", err)
	}

	entry, _ := c.Get("localhost:6309", "/index.md", protocol.VerbFetch)
	if entry == nil {
		t.Fatal("expected cached entry")
	}
	if entry.Response.Body != "# Hello\n" {
		t.Errorf("body: got %q", entry.Response.Body)
	}
}

func TestPathSanitisation(t *testing.T) {
	c := New(t.TempDir())

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{},
		Body:     "# Safe\n",
	}

	// Traversal path should be cleaned
	if err := c.Put("localhost:6309", "/../../etc/passwd", protocol.VerbFetch, resp); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Should not write outside cache dir
	escaped := filepath.Join(c.Dir, "..", "..", "etc", "passwd")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatal("SECURITY: cache wrote outside cache directory")
	}

	// Should be accessible via the cleaned path
	entry, err := c.Get("localhost:6309", "/../../etc/passwd", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if entry == nil {
		t.Fatal("expected cached entry for cleaned path")
	}
}

func TestCorruptedMetaIsCacheMiss(t *testing.T) {
	c := New(t.TempDir())

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"version": "1"},
		Body:     "# Hello\n",
	}

	if err := c.Put("localhost:6309", "/index.md", protocol.VerbFetch, resp); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Corrupt the metadata file
	metaPath := c.filePath("localhost:6309", "/index.md", protocol.VerbFetch) + ".meta"
	if err := os.WriteFile(metaPath, []byte("not valid toml {{{"), 0o644); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}

	entry, err := c.Get("localhost:6309", "/index.md", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("expected no error for corrupted meta, got: %v", err)
	}
	if entry != nil {
		t.Error("expected nil entry for corrupted meta")
	}
}

func TestMissingMetaIsCacheMiss(t *testing.T) {
	c := New(t.TempDir())

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"version": "1"},
		Body:     "# Hello\n",
	}

	if err := c.Put("localhost:6309", "/index.md", protocol.VerbFetch, resp); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Remove the metadata file
	metaPath := c.filePath("localhost:6309", "/index.md", protocol.VerbFetch) + ".meta"
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("remove meta: %v", err)
	}

	entry, err := c.Get("localhost:6309", "/index.md", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("expected no error for missing meta, got: %v", err)
	}
	if entry != nil {
		t.Error("expected nil entry for missing meta")
	}
}

func TestHostTraversalBlocked(t *testing.T) {
	c := New(t.TempDir())

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{},
		Body:     "# Pwned\n",
	}

	if err := c.Put("../../etc", "/passwd", protocol.VerbFetch, resp); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Should not write outside cache dir
	escaped := filepath.Join(c.Dir, "..", "..", "etc", "passwd")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatal("SECURITY: cache wrote outside cache directory via host traversal")
	}

	// Should still be retrievable via the same key
	entry, err := c.Get("../../etc", "/passwd", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if entry == nil {
		t.Fatal("expected cached entry for sanitized host")
	}
}

func TestNestedPath(t *testing.T) {
	c := New(t.TempDir())

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"version": "2"},
		Body:     "# Guide\n",
	}

	if err := c.Put("localhost:6309", "/docs/guide.md", protocol.VerbFetch, resp); err != nil {
		t.Fatalf("put: %v", err)
	}

	entry, err := c.Get("localhost:6309", "/docs/guide.md", protocol.VerbFetch)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if entry == nil {
		t.Fatal("expected cached entry")
	}
	if entry.Response.Metadata["version"] != "2" {
		t.Errorf("version: got %q, want %q", entry.Response.Metadata["version"], "2")
	}
}
