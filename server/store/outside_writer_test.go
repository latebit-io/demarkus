package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// A document another process wrote is missing from the index but spelled
// exactly on disk: a document, not an alias.
func TestOutsideWriteIsNotAnAlias(t *testing.T) {
	root := t.TempDir()
	server := New(root)
	if _, err := server.Write("/old.md", []byte("# Old\n"), nil); err != nil {
		t.Fatal(err)
	}
	if err := server.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	tool := New(root)
	if _, err := tool.Write("/dir/new.md", []byte("# New\n"), nil); err != nil {
		t.Fatal(err)
	}
	doc, err := server.Get("/dir/new.md", 0)
	if err != nil || string(doc.Content) != "# New\n" {
		t.Fatalf("Get of an outside write = %v, %v", doc, err)
	}
	if _, err := server.Get("/dir/new.md", 1); err != nil {
		t.Fatalf("Get v1 of an outside write: %v", err)
	}
	if _, err := server.ReadCurrent("/dir/new.md"); err != nil {
		t.Fatalf("ReadCurrent of an outside write: %v", err)
	}
	if _, err := server.Write("/dir/new.md", []byte("# Newer\n"), nil); err != nil {
		t.Fatalf("write over an outside write: %v", err)
	}
}

func TestGenerationMovesOnMarkChanged(t *testing.T) {
	s := New(t.TempDir())
	before, err := s.Generation()
	if err != nil || !before.Equal(Generation{}) {
		t.Fatalf("Generation of an unmarked root = %v, %v", before, err)
	}
	// Marks in one clock tick, each replacing the last: a reused inode or
	// an equal time must not make any two read as one.
	const marks = 8
	seen := make([]Generation, 0, marks+1)
	seen = append(seen, before)
	for range marks {
		if err := s.MarkChanged(); err != nil {
			t.Fatal(err)
		}
		next, err := s.Generation()
		if err != nil {
			t.Fatal(err)
		}
		for _, earlier := range seen {
			if next.Equal(earlier) {
				t.Fatalf("Generation %v after MarkChanged reads as the earlier %v", next, earlier)
			}
		}
		if again, err := s.Generation(); err != nil || !again.Equal(next) {
			t.Fatalf("an unmoved marker reads as changed: %v then %v, %v", next, again, err)
		}
		seen = append(seen, next)
	}
	// The marker is never a document nor a listed entry.
	if err := s.BuildIndex(); err != nil || s.IndexSize() != 0 {
		t.Fatalf("index over a marked root: size %d, %v", s.IndexSize(), err)
	}
	entries, err := s.ListEntries("/", storefmt.ListOptions{IncludeArchived: true})
	if err != nil || len(entries) != 0 {
		t.Fatalf("listing of a marked root = %v, %v", entries, err)
	}
}

// unreadableVersions makes a document's version directory writable but not
// listable, the shape of a read failure that is not "no such directory".
func unreadableVersions(t *testing.T, root, base string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("needs a directory the process cannot list")
	}
	dir := filepath.Join(root, versionsDirName, base)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // a test directory
			t.Error(err)
		}
	})
}

// A version directory that cannot be read is not "no versions": the conflict
// check fails instead of passing a create-only write over the document.
func TestUnreadableVersionsRefuseTheWrite(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, err := s.Write("/doc.md", []byte("# One\n"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("/doc.md", []byte("# Two\n"), map[string]string{"retention": "1"}); err != nil {
		t.Fatal(err)
	}
	unreadableVersions(t, root, "doc.md")

	doc, err := s.WriteVersion("/doc.md", 0, []byte("# Clobber\n"), nil)
	if err == nil || errors.Is(err, storefmt.ErrConflict) || storefmt.Committed(err) {
		t.Fatalf("create-only write = %+v, %v; want a read failure", doc, err)
	}
	if _, err := s.Write("/doc.md", []byte("# Clobber\n"), nil); err == nil {
		t.Fatal("unconditional write landed over unreadable versions")
	}
	if _, err := s.CurrentVersionResult("/doc.md"); err == nil {
		t.Fatal("CurrentVersionResult read unreadable versions as a version")
	}
	target, err := os.Readlink(filepath.Join(root, "doc.md"))
	if err != nil || filepath.Base(target) != "v2" {
		t.Fatalf("current pointer = %q, %v; want v2", target, err)
	}
}

// The marker read every file-store read pays, with a marker and without.
func BenchmarkGeneration(b *testing.B) {
	for _, marked := range []bool{false, true} {
		name := "unmarked"
		if marked {
			name = "marked"
		}
		b.Run(name, func(b *testing.B) {
			s := New(b.TempDir())
			if marked {
				if err := s.MarkChanged(); err != nil {
					b.Fatal(err)
				}
			}
			for b.Loop() {
				if _, err := s.Generation(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
