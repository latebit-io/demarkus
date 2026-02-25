package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
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
	if !strings.HasPrefix(string(vData), "---\nversion: 1\narchived: false\n---\n") {
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

func TestWrite_CreatesSubdirectory(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// Publish to a path whose parent directory does not exist yet.
	doc, err := s.Write("/newdir/doc.md", []byte("# Hello\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("version: got %d, want 1", doc.Version)
	}

	// Verify the version file was created on disk.
	versionFile := filepath.Join(root, "newdir", "versions", "doc.md.v1")
	if _, err := os.Stat(versionFile); err != nil {
		t.Errorf("version file not found: %v", err)
	}
}

func TestWrite_SubdirectoryTraversal(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// Traversal via nested subdirectory path.
	traversals := []string{
		"/subdir/../../etc/passwd",
		"/newdir/../../../tmp/evil.md",
		"/../outside/doc.md",
	}
	for _, p := range traversals {
		_, err := s.Write(p, []byte("evil"))
		if err == nil {
			t.Errorf("expected error for traversal path %q", p)
		}
	}
}

func TestWrite_SymlinkSubdirectoryEscape(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	s := New(root)

	// Create a symlink inside the content root pointing outside.
	symlinkDir := filepath.Join(root, "escaped")
	if err := os.Symlink(outsideDir, symlinkDir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// Writing through the symlink should be blocked — the resolved path
	// is outside the content root.
	_, err := s.Write("/escaped/doc.md", []byte("# Escaped\n"))
	if err == nil {
		t.Fatal("expected error for symlink escape write")
	}

	// Verify nothing was written outside the root.
	if _, err := os.Stat(filepath.Join(outsideDir, "versions")); err == nil {
		t.Error("directory was created outside content root via symlink escape")
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

func TestWrite_DuplicateContentIsNoOp(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	content := []byte("# Hello\n")

	doc1, err := s.Write("/doc.md", content)
	if err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if doc1.Version != 1 {
		t.Fatalf("version = %d, want 1", doc1.Version)
	}

	// Publishing identical content should return ErrNotModified.
	doc2, err := s.Write("/doc.md", content)
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("expected ErrNotModified, got: %v", err)
	}
	if doc2.Version != 1 {
		t.Errorf("version = %d, want 1", doc2.Version)
	}
	if !bytes.Equal(doc2.Content, content) {
		t.Errorf("content = %q, want %q", doc2.Content, content)
	}

	// No v2 file should exist.
	v2Path := filepath.Join(root, "versions", "doc.md.v2")
	if _, err := os.Stat(v2Path); !os.IsNotExist(err) {
		t.Error("v2 file should not exist for duplicate content")
	}

	// Different content should still create v2.
	doc3, err := s.Write("/doc.md", []byte("# Updated\n"))
	if err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if doc3.Version != 2 {
		t.Errorf("version = %d, want 2", doc3.Version)
	}

	// Publishing the v2 content again should be a no-op.
	doc4, err := s.Write("/doc.md", []byte("# Updated\n"))
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("expected ErrNotModified, got: %v", err)
	}
	if doc4.Version != 2 {
		t.Errorf("version = %d, want 2", doc4.Version)
	}
}

func TestWriteVersion(t *testing.T) {
	t.Run("matching version succeeds", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)

		if _, err := s.Write("/doc.md", []byte("# v1\n")); err != nil {
			t.Fatalf("write v1: %v", err)
		}

		doc, err := s.WriteVersion("/doc.md", 1, []byte("# v2\n"))
		if err != nil {
			t.Fatalf("WriteVersion: %v", err)
		}
		if doc.Version != 2 {
			t.Errorf("version = %d, want 2", doc.Version)
		}
	})

	t.Run("mismatched version returns ErrConflict", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)

		if _, err := s.Write("/doc.md", []byte("# v1\n")); err != nil {
			t.Fatalf("write v1: %v", err)
		}
		if _, err := s.Write("/doc.md", []byte("# v2\n")); err != nil {
			t.Fatalf("write v2: %v", err)
		}

		doc, err := s.WriteVersion("/doc.md", 1, []byte("# stale edit\n"))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict, got: %v", err)
		}
		if doc.Version != 2 {
			t.Errorf("conflict doc version = %d, want 2", doc.Version)
		}
	})

	t.Run("negative expected version skips check", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)

		if _, err := s.Write("/doc.md", []byte("# v1\n")); err != nil {
			t.Fatalf("write v1: %v", err)
		}

		doc, err := s.WriteVersion("/doc.md", -1, []byte("# v2\n"))
		if err != nil {
			t.Fatalf("WriteVersion with -1: %v", err)
		}
		if doc.Version != 2 {
			t.Errorf("version = %d, want 2", doc.Version)
		}
	})

	t.Run("zero expected version creates new document", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)

		doc, err := s.WriteVersion("/new.md", 0, []byte("# Hello\n"))
		if err != nil {
			t.Fatalf("WriteVersion: %v", err)
		}
		if doc.Version != 1 {
			t.Errorf("version = %d, want 1", doc.Version)
		}
	})

	t.Run("zero expected version conflicts if document exists", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)

		if _, err := s.Write("/doc.md", []byte("# v1\n")); err != nil {
			t.Fatalf("write v1: %v", err)
		}

		doc, err := s.WriteVersion("/doc.md", 0, []byte("# should conflict\n"))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict, got: %v", err)
		}
		if doc.Version != 1 {
			t.Errorf("conflict doc version = %d, want 1", doc.Version)
		}
	})

	t.Run("concurrent create race returns ErrConflict", func(t *testing.T) {
		// Two writers both try to create a new document (expectedVersion=0).
		// Writer A wins and creates v1. Writer B's pre-check now sees
		// current=1 != 0, returning ErrConflict.
		//
		// The tighter O_EXCL race (both pass pre-check, one loses the
		// file create) and the post-check race (version jump detection)
		// cannot be triggered deterministically without injecting hooks
		// between WriteVersion's pre-check and Write call. These paths
		// are tested indirectly: TestWrite_ImmutabilityGuard verifies
		// Write returns ErrVersionExists on O_EXCL collision, and the
		// post-check is a defensive guard for the same class of race.
		root := t.TempDir()
		s := New(root)

		// Writer A wins.
		doc, err := s.WriteVersion("/doc.md", 0, []byte("# writer A\n"))
		if err != nil {
			t.Fatalf("writer A: %v", err)
		}
		if doc.Version != 1 {
			t.Errorf("writer A version = %d, want 1", doc.Version)
		}

		// Writer B arrives with stale expectedVersion=0.
		doc, err = s.WriteVersion("/doc.md", 0, []byte("# writer B\n"))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("writer B: expected ErrConflict, got: %v", err)
		}
		if doc.Version != 1 {
			t.Errorf("writer B conflict version = %d, want 1", doc.Version)
		}
	})

	t.Run("not-modified at expected version passes through", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)

		content := []byte("# Hello\n")
		if _, err := s.Write("/doc.md", content); err != nil {
			t.Fatalf("write v1: %v", err)
		}

		// Publishing identical content with correct expectedVersion
		// should return ErrNotModified (not ErrConflict).
		doc, err := s.WriteVersion("/doc.md", 1, content)
		if !errors.Is(err, ErrNotModified) {
			t.Fatalf("expected ErrNotModified, got: %v", err)
		}
		if doc.Version != 1 {
			t.Errorf("version = %d, want 1", doc.Version)
		}
	})
}

func TestArchive(t *testing.T) {
	setup := func(t *testing.T) (*Store, string) {
		t.Helper()
		root := t.TempDir()
		s := New(root)
		if _, err := s.Write("/doc.md", []byte("# Hello\n")); err != nil {
			t.Fatalf("setup Write: %v", err)
		}
		return s, root
	}

	t.Run("archive document", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.Archive("/doc.md", true); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		doc, err := s.Get("/doc.md", 0)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !doc.Archived {
			t.Error("expected doc to be archived")
		}
	})

	t.Run("unarchive document", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.Archive("/doc.md", true); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		if err := s.Archive("/doc.md", false); err != nil {
			t.Fatalf("Unarchive: %v", err)
		}
		doc, err := s.Get("/doc.md", 0)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if doc.Archived {
			t.Error("expected doc to be unarchived")
		}
	})

	t.Run("archive already archived", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.Archive("/doc.md", true); err != nil {
			t.Fatalf("Archive first: %v", err)
		}
		if err := s.Archive("/doc.md", true); err != nil {
			t.Fatalf("Archive second: %v", err)
		}
		doc, err := s.Get("/doc.md", 0)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !doc.Archived {
			t.Error("expected doc to remain archived")
		}
	})

	t.Run("unarchive already active", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.Archive("/doc.md", false); err != nil {
			t.Fatalf("Unarchive: %v", err)
		}
		doc, err := s.Get("/doc.md", 0)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if doc.Archived {
			t.Error("expected doc to remain active")
		}
	})

	t.Run("not found", func(t *testing.T) {
		s, _ := setup(t)
		err := s.Archive("/missing.md", true)
		if !os.IsNotExist(err) {
			t.Errorf("expected not-exist error, got: %v", err)
		}
	})

	t.Run("path traversal", func(t *testing.T) {
		s, _ := setup(t)
		err := s.Archive("/../etc/passwd", true)
		if !os.IsNotExist(err) {
			t.Errorf("expected not-exist error for traversal, got: %v", err)
		}
	})

	t.Run("version pinning ignores archive flag", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.Archive("/doc.md", true); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		doc, err := s.Get("/doc.md", 1)
		if err != nil {
			t.Fatalf("Get v1: %v", err)
		}
		if doc.Version != 1 {
			t.Errorf("version = %d, want 1", doc.Version)
		}
		if !strings.Contains(string(doc.Content), "# Hello") {
			t.Errorf("expected content to contain '# Hello', got: %q", doc.Content)
		}
	})

	t.Run("hash chain valid after archive", func(t *testing.T) {
		s, _ := setup(t)
		if _, err := s.Write("/doc.md", []byte("# V2\n")); err != nil {
			t.Fatalf("Write v2: %v", err)
		}
		if err := s.Archive("/doc.md", true); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		// Unarchive and write v3 — the chain should still verify because
		// v3's previous-hash covers v2's on-disk bytes (including archived flag).
		if err := s.Archive("/doc.md", false); err != nil {
			t.Fatalf("Unarchive: %v", err)
		}
		if _, err := s.Write("/doc.md", []byte("# V3\n")); err != nil {
			t.Fatalf("Write v3: %v", err)
		}
		if err := s.VerifyChain("/doc.md"); err != nil {
			t.Errorf("chain verification failed after archive cycle: %v", err)
		}
	})

	t.Run("write rejected on archived document", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.Archive("/doc.md", true); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		_, err := s.Write("/doc.md", []byte("# New content\n"))
		if err != ErrArchived {
			t.Errorf("expected ErrArchived, got: %v", err)
		}
	})
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
