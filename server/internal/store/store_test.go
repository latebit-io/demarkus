package store

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGet_FlatFileRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("# Hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	_, err := s.Get("/doc.md", 0)
	if err == nil {
		t.Fatal("expected error: flat files without version history should not be served")
	}
}

func TestGet_VersionedFile(t *testing.T) {
	root := t.TempDir()
	versionsDir := filepath.Join(root, "versions")
	if err := os.Mkdir(versionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionsDir, "doc.md.v1"), []byte("# V1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionsDir, "doc.md.v2"), []byte("# V2"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Current file (would be a symlink in production)
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("# V2"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(root)

	// Get current version
	doc, err := s.Get("/doc.md", 0)
	if err != nil {
		t.Fatalf("Get current: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("current version = %d, want 2", doc.Version)
	}

	// Get specific version
	doc, err = s.Get("/doc.md", 1)
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	if string(doc.Content) != "# V1" {
		t.Errorf("v1 content = %q, want %q", doc.Content, "# V1")
	}
	if doc.Version != 1 {
		t.Errorf("v1 version = %d, want 1", doc.Version)
	}
}

func TestGet_NotFound(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	_, err := s.Get("/missing.md", 0)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestGet_PathTraversal(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	_, err := s.Get("/../etc/passwd", 0)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestGet_Directory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(root)

	_, err := s.Get("/subdir", 0)
	if err == nil {
		t.Fatal("expected error for directory")
	}
}

func TestListDir(t *testing.T) {
	root := t.TempDir()
	for _, f := range []struct{ name, content string }{
		{"a.md", "a"}, {"b.md", "b"}, {".hidden", "hidden"},
	} {
		if err := os.WriteFile(filepath.Join(root, f.name), []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "versions"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	entries, err := s.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2 (excluding .hidden and versions)", len(entries))
	}
}

func TestListDir_NotADirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(root)

	_, err := s.ListDir("/file.md")
	if err == nil {
		t.Fatal("expected error for file")
	}
}

func TestVersions_FlatFileRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("# Hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	_, err := s.Versions("/doc.md")
	if err == nil {
		t.Fatal("expected error: flat files without version history should not be served")
	}
}

func TestVersions_MultipleVersions(t *testing.T) {
	root := t.TempDir()
	versionsDir := filepath.Join(root, "versions")
	if err := os.Mkdir(versionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ name, content string }{
		{filepath.Join(root, "doc.md"), "current"},
		{filepath.Join(versionsDir, "doc.md.v1"), "v1"},
		{filepath.Join(versionsDir, "doc.md.v2"), "v2"},
		{filepath.Join(versionsDir, "doc.md.v3"), "v3"},
	} {
		if err := os.WriteFile(f.name, []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := New(root)
	versions, err := s.Versions("/doc.md")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions count = %d, want 3", len(versions))
	}
	// Should be newest first.
	if versions[0].Version != 3 {
		t.Errorf("first version = %d, want 3", versions[0].Version)
	}
	if versions[2].Version != 1 {
		t.Errorf("last version = %d, want 1", versions[2].Version)
	}
}

func TestVersions_NotFound(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	_, err := s.Versions("/missing.md")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestGetVersion_NotFound(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	_, err := s.Get("/doc.md", 99)
	if err == nil {
		t.Fatal("expected error for missing version")
	}
}

func TestWrite_NewDocument(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	doc, err := s.Write("/new.md", []byte("# Hello\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
	if string(doc.Content) != "# Hello\n" {
		t.Errorf("content = %q, want %q", doc.Content, "# Hello\n")
	}

	// Version file should exist with store frontmatter.
	vData, err := os.ReadFile(filepath.Join(root, "versions", "new.md.v1"))
	if err != nil {
		t.Fatalf("read version file: %v", err)
	}
	if !strings.HasPrefix(string(vData), "---\nversion: 1\n---\n") {
		t.Errorf("v1 should have store frontmatter without previous-hash, got: %q", string(vData))
	}

	// Current file should match version file.
	cData, err := os.ReadFile(filepath.Join(root, "new.md"))
	if err != nil {
		t.Fatalf("read current file: %v", err)
	}
	if !bytes.Equal(cData, vData) {
		t.Errorf("current file should match version file")
	}
}

func TestWrite_CreatesVersion(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// Write v1 through the protocol.
	if _, err := s.Write("/doc.md", []byte("# V1\n")); err != nil {
		t.Fatalf("Write v1: %v", err)
	}

	// Write v2.
	doc, err := s.Write("/doc.md", []byte("# V2\n"))
	if err != nil {
		t.Fatalf("Write v2: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("version = %d, want 2", doc.Version)
	}

	// versions/doc.md.v2 must exist.
	if _, err := os.Stat(filepath.Join(root, "versions", "doc.md.v2")); err != nil {
		t.Errorf("version file not created: %v", err)
	}
}

func TestWrite_Increments(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	for i := 1; i <= 3; i++ {
		doc, err := s.Write("/doc.md", fmt.Appendf(nil, "# V%d\n", i))
		if err != nil {
			t.Fatalf("Write v%d: %v", i, err)
		}
		if doc.Version != i {
			t.Errorf("version = %d, want %d", doc.Version, i)
		}
	}

	// All three version files must exist.
	for i := 1; i <= 3; i++ {
		path := filepath.Join(root, "versions", fmt.Sprintf("doc.md.v%d", i))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing version file v%d: %v", i, err)
		}
	}
}

func TestWrite_PathTraversal(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	_, err := s.Write("/../etc/passwd", []byte("evil"))
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestWrite_TooLarge(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	big := make([]byte, MaxFileSize+1)
	_, err := s.Write("/doc.md", big)
	if err == nil {
		t.Fatal("expected error for oversized content")
	}
}

func TestWrite_ImmutabilityGuard(t *testing.T) {
	root := t.TempDir()
	versionsDir := filepath.Join(root, "versions")
	if err := os.MkdirAll(versionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No doc.md on disk → next=1. Pre-create v1 to simulate a concurrent writer
	// that won the race and already wrote v1 before we get to the atomic rename.
	if err := os.WriteFile(filepath.Join(versionsDir, "doc.md.v1"), []byte("# already there\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	_, err := s.Write("/doc.md", []byte("# New\n"))
	if err == nil {
		t.Fatal("expected error: version 1 already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWrite_HashChain(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// Write v1.
	if _, err := s.Write("/doc.md", []byte("# V1\n")); err != nil {
		t.Fatalf("write v1: %v", err)
	}

	// Write v2.
	if _, err := s.Write("/doc.md", []byte("# V2\n")); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	v1Data, err := os.ReadFile(filepath.Join(root, "versions", "doc.md.v1"))
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	v2Data, err := os.ReadFile(filepath.Join(root, "versions", "doc.md.v2"))
	if err != nil {
		t.Fatalf("read v2: %v", err)
	}

	// v1 must not have previous-hash.
	if strings.Contains(string(v1Data), "previous-hash") {
		t.Errorf("v1 should not have previous-hash, got: %q", string(v1Data))
	}

	// v2 must have previous-hash matching sha256 of v1 raw bytes.
	h := sha256.Sum256(v1Data)
	expected := fmt.Sprintf("sha256-%x", h)
	if !strings.Contains(string(v2Data), "previous-hash: "+expected) {
		t.Errorf("v2 previous-hash mismatch\nwant: %s\ngot:  %s", expected, string(v2Data))
	}
}

func TestVerifyChain_Valid(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	for i := 1; i <= 3; i++ {
		if _, err := s.Write("/doc.md", fmt.Appendf(nil, "# V%d\n", i)); err != nil {
			t.Fatalf("write v%d: %v", i, err)
		}
	}

	if err := s.VerifyChain("/doc.md"); err != nil {
		t.Errorf("expected valid chain, got error: %v", err)
	}
}

func TestVerifyChain_Tampered(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	if _, err := s.Write("/doc.md", []byte("# V1\n")); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if _, err := s.Write("/doc.md", []byte("# V2\n")); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	// Tamper with v1 after the chain is formed.
	v1Path := filepath.Join(root, "versions", "doc.md.v1")
	if err := os.WriteFile(v1Path, []byte("# TAMPERED\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := s.VerifyChain("/doc.md")
	if err == nil {
		t.Fatal("expected chain verification error after tampering")
	}
	if !strings.Contains(err.Error(), "chain broken") {
		t.Errorf("unexpected error: %v", err)
	}
}
