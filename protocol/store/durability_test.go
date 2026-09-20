package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWriteSyncsFileAndDirectories(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	var files, dirs []string
	syncFile = func(f *os.File) error { files = append(files, f.Name()); return f.Sync() }
	syncDir = func(dir string) error { dirs = append(dirs, dir); return syncDirectory(dir) }
	t.Cleanup(func() { syncFile = (*os.File).Sync; syncDir = syncDirectory })

	if _, err := s.Write("/notes/a.md", []byte("# A\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	vFile := newVersionFilePath(filepath.Join(root, "notes", "versions"), "a.md", 1)
	if !slices.Contains(files, vFile) {
		t.Errorf("version file not synced; synced %v", files)
	}
	// The version entry and the current pointer live in different directories.
	for _, want := range []string{filepath.Dir(vFile), filepath.Join(root, "notes")} {
		if !slices.Contains(dirs, want) {
			t.Errorf("directory %s not synced; synced %v", want, dirs)
		}
	}
}

func TestWriteFailedFileSyncLeavesNoVersion(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	syncFile = func(*os.File) error { return errors.New("disk gone") }
	t.Cleanup(func() { syncFile = (*os.File).Sync })

	if _, err := s.Write("/a.md", []byte("# A\n"), nil); err == nil {
		t.Fatal("write: want the sync error")
	}
	vFile := newVersionFilePath(filepath.Join(root, "versions"), "a.md", 1)
	if _, err := os.Lstat(vFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unsynced version file left behind: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "a.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("current pointer moved despite failed sync: %v", err)
	}
}
