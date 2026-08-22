package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationPreservesArchiveStateSeparately(t *testing.T) {
	t.Run("export and import archived document", func(t *testing.T) {
		source := New(t.TempDir())
		if _, err := source.Write("/doc.md", []byte("# Body\n"), nil); err != nil {
			t.Fatal(err)
		}
		versionFile, err := source.VersionFilePath("/doc.md", 1)
		if err != nil {
			t.Fatal(err)
		}
		storedBefore, err := os.ReadFile(versionFile)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := source.Archive("/doc.md", true); err != nil {
			t.Fatal(err)
		}

		exported := make(map[string]StoredDocument)
		if err := source.ExportDocs(context.Background(), func(path string, document StoredDocument) error {
			exported[path] = document
			return nil
		}); err != nil {
			t.Fatalf("ExportDocs: %v", err)
		}
		document := exported["/doc.md"]
		if !document.Archived || len(document.Versions) != 1 || !bytes.Equal(document.Versions[0].Stored, storedBefore) {
			t.Fatalf("exported document = %+v, want archived with original stored bytes", document)
		}

		destinationRoot := t.TempDir()
		destination := New(destinationRoot)
		if err := destination.ImportDoc(context.Background(), "/doc.md", document); err != nil {
			t.Fatalf("ImportDoc: %v", err)
		}
		state, err := os.ReadFile(filepath.Join(destinationRoot, "versions", "doc.md", archiveStateName))
		if err != nil || string(state) != "true\n" {
			t.Fatalf("archive state = %q (err %v), want true\\n", state, err)
		}
		current, err := destination.Get("/doc.md", 0)
		if err != nil || !current.Archived {
			t.Fatalf("current = %+v (err %v), want archived", current, err)
		}
		pinned, err := destination.Get("/doc.md", 1)
		if err != nil || pinned.Archived {
			t.Fatalf("pinned = %+v (err %v), want unarchived", pinned, err)
		}
		if _, err := destination.LookupHash(ContentHash([]byte("# Body\n"))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("archived imported hash lookup err = %v, want ErrNotExist", err)
		}
	})

	t.Run("explicit false overrides legacy tip", func(t *testing.T) {
		stored, err := SerializeVersion(1, nil, []byte("# Legacy\n"), nil)
		if err != nil {
			t.Fatal(err)
		}
		stored, err = SetArchived(stored, true)
		if err != nil {
			t.Fatal(err)
		}
		document := StoredDocument{
			Versions: []StoredVersion{{Version: 1, Stored: stored, Modified: time.Now()}},
			Archived: false,
		}
		root := t.TempDir()
		destination := New(root)
		if err := destination.ImportDoc(context.Background(), "/legacy.md", document); err != nil {
			t.Fatalf("ImportDoc: %v", err)
		}
		state, err := os.ReadFile(filepath.Join(root, "versions", "legacy.md", archiveStateName))
		if err != nil || string(state) != "false\n" {
			t.Fatalf("archive state = %q (err %v), want false\\n", state, err)
		}
		current, err := destination.Get("/legacy.md", 0)
		if err != nil || current.Archived {
			t.Fatalf("current = %+v (err %v), want active", current, err)
		}
		versionFile, err := destination.VersionFilePath("/legacy.md", 1)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := os.ReadFile(versionFile)
		if err != nil || !bytes.Equal(imported, stored) {
			t.Errorf("imported bytes equal = %v (err %v), want true", bytes.Equal(imported, stored), err)
		}
		if path, err := destination.LookupHash(ContentHash([]byte("# Legacy\n"))); err != nil || path != "/legacy.md" {
			t.Errorf("active imported hash lookup = (%q, %v), want /legacy.md", path, err)
		}

		exported := make(map[string]StoredDocument)
		if err := destination.ExportDocs(context.Background(), func(path string, document StoredDocument) error {
			exported[path] = document
			return nil
		}); err != nil {
			t.Fatalf("ExportDocs: %v", err)
		}
		if err := DiffExports(map[string]StoredDocument{"/legacy.md": document}, exported); err != nil {
			t.Fatalf("DiffExports: %v", err)
		}
	})
}

func TestDiffExportsComparesArchiveState(t *testing.T) {
	version := StoredVersion{Version: 1, Stored: []byte("stored"), Modified: time.Now()}
	want := map[string]StoredDocument{"/doc.md": {Versions: []StoredVersion{version}, Archived: true}}
	got := map[string]StoredDocument{"/doc.md": {Versions: []StoredVersion{version}, Archived: false}}
	if err := DiffExports(want, got); err == nil {
		t.Fatal("DiffExports accepted different archive state")
	}
}
