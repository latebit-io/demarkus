package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// WriteFileAtomic replaces path through a synced sibling temp file, a rename
// and a directory sync. committed true with an error: the file is in place,
// its directory entry not yet known durable.
func WriteFileAtomic(path string, mode os.FileMode, write func(w io.Writer) error) (committed bool, retErr error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "." // a bare name: keep the temp on the same filesystem
	}
	f, err := os.CreateTemp(dir, "."+base+"-*")
	if err != nil {
		return false, fmt.Errorf("create temp %s: %w", base, err)
	}
	tmpPath := f.Name()
	// After success the file is closed and renamed; both cleanups then
	// report the state they find and are skipped.
	defer func() {
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			retErr = errors.Join(retErr, fmt.Errorf("close temp %s: %w", base, err))
		}
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove temp %s: %w", base, err))
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return false, fmt.Errorf("chmod temp %s: %w", base, err)
	}
	if err := write(f); err != nil {
		return false, fmt.Errorf("write temp %s: %w", base, err)
	}
	if err := syncFile(f); err != nil {
		return false, fmt.Errorf("sync temp %s: %w", base, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("close temp %s: %w", base, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("rename %s: %w", base, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return true, fmt.Errorf("sync directory of %s: %w", base, err)
	}
	return true, nil
}

// syncFile, syncDir and statFile are the fallible file calls; tests replace them.
var (
	syncFile = (*os.File).Sync
	syncDir  = syncDirectory
	statFile = (*os.File).Stat
)

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync directory %s: %w", dir, err), d.Close())
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close directory %s: %w", dir, err)
	}
	return nil
}

// dirSyncDebt holds directory syncs still owed: a sync failed and the created
// directory could not be removed again, so a retry would find it present.
type dirSyncDebt struct {
	mu   sync.Mutex
	owed map[string]struct{}
}

func (d *dirSyncDebt) add(dirs []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.owed == nil {
		d.owed = make(map[string]struct{})
	}
	for _, dir := range dirs {
		d.owed[dir] = struct{}{}
	}
}

// settle syncs what is owed; a directory stays owed until its sync succeeds.
func (d *dirSyncDebt) settle() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for dir := range d.owed {
		// A directory the rollback removed has no entries left to make durable.
		if err := syncDir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("settle owed directory sync: %w", err)
		}
		delete(d.owed, dir)
	}
	return nil
}

// mkdirAllDurable is MkdirAll plus a sync of every directory that gained an
// entry, so a crash cannot drop the new tree while the pointer survives. A
// failed sync undoes the creation. An existing path costs one Lstat.
func (s *Store) mkdirAllDurable(dir string) error {
	if err := s.dirDebt.settle(); err != nil {
		return err
	}
	var created []string
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
			break
		}
		created = append(created, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	if len(created) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // read-only installs publish as root, serve as another user
		return err
	}
	// Shallowest first: each created directory is an entry in its parent.
	for _, p := range slices.Backward(created) {
		if err := syncDir(filepath.Dir(p)); err != nil {
			undoErr := removeCreated(created)
			if undoErr != nil {
				// The tree stays, so every parent sync stays owed.
				s.dirDebt.add(parentsOf(created))
			}
			return errors.Join(err, undoErr)
		}
	}
	return nil
}

func parentsOf(dirs []string) []string {
	parents := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		parents = append(parents, filepath.Dir(dir))
	}
	return parents
}

// removeCreated undoes a directory creation whose sync failed, deepest first,
// so a retry creates and syncs them again instead of finding them present.
// A directory another writer already filled stays and is reported.
func removeCreated(created []string) error {
	var errs []error
	for _, p := range created {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("undo unsynced directory %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// createVersionFile writes and syncs a new version and returns its mtime, read
// here so nothing fallible sits between the pointer swap and the index update.
// O_EXCL is the immutability guard: an existing file fails the create.
func createVersionFile(vFile string, stored []byte, version int) (time.Time, error) {
	f, err := os.OpenFile(vFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // read-only installs publish as root, serve as another user
	if err != nil {
		if os.IsExist(err) {
			return time.Time{}, fmt.Errorf("version %d: %w", version, storefmt.ErrVersionExists)
		}
		return time.Time{}, fmt.Errorf("create version file: %w", err)
	}
	if _, err := f.Write(stored); err != nil {
		return time.Time{}, errors.Join(fmt.Errorf("write version file: %w", err), f.Close(), removeIfPresent(vFile))
	}
	if err := syncFile(f); err != nil {
		return time.Time{}, errors.Join(fmt.Errorf("sync version file: %w", err), f.Close(), removeIfPresent(vFile))
	}
	info, err := statFile(f)
	if err != nil {
		return time.Time{}, errors.Join(fmt.Errorf("stat version file: %w", err), f.Close(), removeIfPresent(vFile))
	}
	if err := f.Close(); err != nil {
		return time.Time{}, errors.Join(fmt.Errorf("close version file: %w", err), removeIfPresent(vFile))
	}
	if err := syncDir(filepath.Dir(vFile)); err != nil {
		return time.Time{}, errors.Join(err, removeIfPresent(vFile))
	}
	return info.ModTime(), nil
}

// swapCurrent points the current file at relTarget by renaming a temp symlink
// over it, so readers never see it missing (relative, so the root can move).
// applied false means the pointer did not move and the version is unreferenced.
func swapCurrent(currentFile, relTarget string) (applied bool, err error) {
	tmpLink := currentFile + ".tmp"
	if err := removeIfPresent(tmpLink); err != nil {
		return false, fmt.Errorf("clear stale temp link: %w", err)
	}
	if err := os.Symlink(relTarget, tmpLink); err != nil {
		return false, fmt.Errorf("symlink current file: %w", err)
	}
	if err := os.Rename(tmpLink, currentFile); err != nil {
		return false, errors.Join(fmt.Errorf("rename current file: %w", err), removeIfPresent(tmpLink))
	}
	return true, syncDir(filepath.Dir(currentFile))
}

func removeIfPresent(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}
