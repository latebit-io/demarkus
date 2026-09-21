package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// outsideDoc builds a valid document in a second store and links its parent
// directory into root as "linked", so /linked/doc.md resolves outside root.
func outsideDoc(t *testing.T, root string) {
	t.Helper()
	outside := t.TempDir()
	if _, err := New(outside).Write("/doc.md", []byte("# Outside\n"), nil); err != nil {
		t.Fatalf("seed outside doc: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func TestVersionQuestionsStayInsideRoot(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	outsideDoc(t, root)

	if v, err := s.CurrentVersionResult("/linked/doc.md"); v != 0 || err != nil {
		t.Errorf("CurrentVersionResult = (%d, %v) for an escaping symlink, want (0, nil)", v, err)
	}
	if p, err := s.VersionFilePath("/linked/doc.md", 1); err == nil {
		t.Errorf("VersionFilePath returned %q for an escaping path", p)
	}
	if err := s.VerifyChain("/linked/doc.md"); err == nil {
		t.Error("VerifyChain verified a chain outside the root")
	}
	if versions := s.findVersions("/linked/doc.md"); len(versions) != 0 {
		t.Errorf("findVersions listed %d versions outside the root", len(versions))
	}
}

func TestImportDocRefusesEscapingDirectory(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	stored, err := storefmt.SerializeVersion(1, nil, []byte("# Imported\n"), nil)
	if err != nil {
		t.Fatalf("SerializeVersion: %v", err)
	}
	doc := storefmt.StoredDocument{Versions: []storefmt.StoredVersion{{Version: 1, Stored: stored, Modified: time.Now()}}}

	if err := s.ImportDoc(context.Background(), "/linked/new.md", doc); err == nil {
		t.Error("ImportDoc wrote through an escaping symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("import left %d entries outside the root", len(entries))
	}
}

func TestGetVersionRefusesPlantedSymlink(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, err := s.Write("/doc.md", []byte("# One\n"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := s.Write("/other.md", []byte("# Other\n"), nil); err != nil {
		t.Fatalf("Write other: %v", err)
	}
	target, err := s.VersionFilePath("/other.md", 1)
	if err != nil {
		t.Fatalf("VersionFilePath: %v", err)
	}
	planted, err := s.VersionFilePath("/doc.md", 7)
	if err != nil {
		t.Fatalf("VersionFilePath: %v", err)
	}
	if err := os.Symlink(target, planted); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if doc, err := s.Get("/doc.md", 7); err == nil {
		t.Errorf("Get served a planted symlink as version 7: %q", doc.Content)
	}
}
