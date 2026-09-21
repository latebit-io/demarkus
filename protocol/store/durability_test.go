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

// A swap that never happened must not leave an orphan version: the next
// write would hit it with O_EXCL and the document would stay unwritable.
func TestWriteFailedSwapLeavesDocumentWritable(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// A non empty directory where the temp link goes makes the swap fail early.
	blocker := filepath.Join(root, "a.md.tmp")
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("/a.md", []byte("# A\n"), nil); err == nil {
		t.Fatal("write: want the swap error")
	}
	vFile := newVersionFilePath(filepath.Join(root, "versions"), "a.md", 1)
	if _, err := os.Lstat(vFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan version file left behind: %v", err)
	}

	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Write("/a.md", []byte("# A\n"), nil)
	if err != nil {
		t.Fatalf("write after the obstacle is gone: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
}

// Once the pointer moved, a failed directory sync must keep the version file.
func TestWriteFailedSyncAfterSwapKeepsVersion(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// Only the sync after the pointer moved fails; root is also synced
	// earlier, when the versions directory is created under it.
	syncDir = func(dir string) error {
		if _, err := os.Lstat(filepath.Join(root, "a.md")); dir == root && err == nil {
			return errors.New("disk gone")
		}
		return syncDirectory(dir)
	}
	t.Cleanup(func() { syncDir = syncDirectory })

	if _, err := s.Write("/a.md", []byte("# A\n"), nil); err == nil {
		t.Fatal("write: want the sync error")
	}
	if _, err := s.Get("/a.md", 0); err != nil {
		t.Errorf("current pointer dangles after a late sync failure: %v", err)
	}
}

// Every directory a first write creates is an entry in its parent; an
// unsynced parent can lose the version tree while the pointer survives.
func TestWriteSyncsParentsOfCreatedDirectories(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	var dirs []string
	syncDir = func(dir string) error { dirs = append(dirs, dir); return syncDirectory(dir) }
	t.Cleanup(func() { syncDir = syncDirectory })

	if _, err := s.Write("/notes/sub/a.md", []byte("# A\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, want := range []string{
		root,                                // owns notes
		filepath.Join(root, "notes"),        // owns sub
		filepath.Join(root, "notes", "sub"), // owns versions and the pointer
		filepath.Join(root, "notes", "sub", "versions"), // owns the a.md version dir
	} {
		if !slices.Contains(dirs, want) {
			t.Errorf("directory %s not synced; synced %v", want, dirs)
		}
	}

	// A later write creates nothing, so it syncs nothing above the document.
	dirs = nil
	if _, err := s.Write("/notes/sub/a.md", []byte("# A2\n"), nil); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if slices.Contains(dirs, root) || slices.Contains(dirs, filepath.Join(root, "notes")) {
		t.Errorf("steady state write synced ancestors: %v", dirs)
	}
}

// A failed parent sync must not be forgotten: the retry has to sync every
// directory the first attempt created, not find them present and skip.
func TestWriteRetryRedoesFailedDirectorySyncs(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	failing := true
	var dirs []string
	syncDir = func(dir string) error {
		if failing && dir == filepath.Join(root, "notes") {
			return errors.New("disk gone")
		}
		dirs = append(dirs, dir)
		return syncDirectory(dir)
	}
	t.Cleanup(func() { syncDir = syncDirectory })

	if _, err := s.Write("/notes/sub/a.md", []byte("# A\n"), nil); err == nil {
		t.Fatal("write: want the sync error")
	}

	failing = false
	dirs = nil
	if _, err := s.Write("/notes/sub/a.md", []byte("# A\n"), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, want := range []string{
		root,
		filepath.Join(root, "notes"),
		filepath.Join(root, "notes", "sub"),
		filepath.Join(root, "notes", "sub", "versions"),
	} {
		if !slices.Contains(dirs, want) {
			t.Errorf("retry did not sync %s; synced %v", want, dirs)
		}
	}
}

// When the rollback cannot remove a directory, the failed syncs stay owed:
// a retry finds the tree present and would otherwise skip them.
func TestWriteRetrySettlesSyncsOwedAfterFailedRollback(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	failing := true
	var dirs []string
	syncDir = func(dir string) error {
		if failing && dir == filepath.Join(root, "notes") {
			// A stray file keeps the rollback from removing the new tree.
			stray := filepath.Join(root, "notes", "sub", "versions", "stray")
			if err := os.WriteFile(stray, nil, 0o600); err != nil {
				t.Fatalf("plant stray file: %v", err)
			}
			return errors.New("disk gone")
		}
		dirs = append(dirs, dir)
		return syncDirectory(dir)
	}
	t.Cleanup(func() { syncDir = syncDirectory })

	if _, err := s.Write("/notes/sub/a.md", []byte("# A\n"), nil); err == nil {
		t.Fatal("write: want the sync error")
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "sub", "versions")); err != nil {
		t.Fatalf("test setup: the tree should have survived the rollback: %v", err)
	}

	failing = false
	dirs = nil
	if _, err := s.Write("/notes/sub/a.md", []byte("# A\n"), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, want := range []string{root, filepath.Join(root, "notes"), filepath.Join(root, "notes", "sub")} {
		if !slices.Contains(dirs, want) {
			t.Errorf("retry did not settle the owed sync of %s; synced %v", want, dirs)
		}
	}
}

// A partial rollback can remove a directory whose sync is owed. The missing
// directory has nothing left to sync and must not block every later write.
func TestWriteSettlesDebtForDirectoryRemovedByRollback(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	failing := true
	syncDir = func(dir string) error {
		if failing && dir == root {
			// The stray file keeps notes; the rollback still removes sub.
			if err := os.WriteFile(filepath.Join(root, "notes", "stray"), nil, 0o600); err != nil {
				t.Fatalf("plant stray file: %v", err)
			}
			return errors.New("disk gone")
		}
		return syncDirectory(dir)
	}
	t.Cleanup(func() { syncDir = syncDirectory })

	if _, err := s.Write("/notes/sub/a.md", []byte("# A\n"), nil); err == nil {
		t.Fatal("write: want the sync error")
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "sub")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test setup: sub should be gone after the partial rollback: %v", err)
	}

	failing = false
	if _, err := s.Write("/other.md", []byte("# O\n"), nil); err != nil {
		t.Fatalf("unrelated write blocked by stale debt: %v", err)
	}
	if _, err := s.Write("/notes/sub/a.md", []byte("# A\n"), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
