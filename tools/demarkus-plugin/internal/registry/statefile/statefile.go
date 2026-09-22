// Package statefile is the transactional engine under every ~/.demarkus and
// MCP config mutation: an mkdir lock, atomic writes, and a prepare-then-apply
// mutation set that rolls every file back when one write fails.
package statefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
)

// WithLock runs fn while holding an atomic mkdir mutex for path+".lock".
// Bounded ~2s.
func WithLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return lockdir.WithLock(path+".lock", 100, 20*time.Millisecond, fn)
}

// Write writes data to path via a temp file + rename (no torn writes).
func Write(path string, data []byte) error { return WriteFile(path, data, 0o644) }

// WriteFile is Write with an explicit mode. CreateTemp starts at
// 0600, so secrets are never briefly world-readable before the exact chmod.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	// O_CREATE perm is masked by umask; enforce the exact mode for secrets.
	if err := os.Chmod(tmp, perm); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}

// slugSafe is the path-safe alphabet a slug must satisfy (it's embedded in mirror
// file paths and compared against MCP server names).
var slugSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidSlug reports whether slug is safe to embed in state file names.
func ValidSlug(slug string) bool { return slugSafe.MatchString(slug) }

// ValidateField rejects a record field that would break the line or tab framing.
func ValidateField(name, value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not start or end with whitespace", name)
	}
	if strings.ContainsAny(value, "\t\r\n\x00") {
		return fmt.Errorf("%s must not contain tab, line break, or NUL delimiters", name)
	}
	return nil
}

type fileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

// Mutation is one prepared file write or delete with the snapshot its rollback restores.
type Mutation struct {
	before fileSnapshot
	data   []byte
	mode   os.FileMode
	delete bool
}

// Writer performs Apply's writes; tests inject failures through it.
var Writer = WriteFile

// Put prepares a write of data to path with mode.
func Put(path string, data []byte, mode os.FileMode) (Mutation, error) {
	snapshot, err := snapshotFile(path)
	if err != nil {
		return Mutation{}, err
	}
	return Mutation{before: snapshot, data: data, mode: mode}, nil
}

// Delete prepares the removal of path; an absent file is a no-op.
func Delete(path string) (Mutation, error) {
	snapshot, err := snapshotFile(path)
	if err != nil {
		return Mutation{}, err
	}
	return Mutation{before: snapshot, delete: true}, nil
}

func snapshotFile(path string) (fileSnapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{path: path}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("snapshot %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("stat snapshot %s: %w", path, err)
	}
	return fileSnapshot{path: path, data: data, mode: info.Mode().Perm(), exists: true}, nil
}

// Apply performs the mutations in order and rolls every one back on the first failure.
func Apply(mutations []Mutation) error {
	for i := range mutations {
		mutation := &mutations[i]
		var err error
		if mutation.delete {
			err = os.Remove(mutation.before.path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else {
			err = Writer(mutation.before.path, mutation.data, mutation.mode)
		}
		if err != nil {
			rollbackErr := rollbackStateMutations(mutations[:i+1])
			return errors.Join(fmt.Errorf("mutate %s: %w", mutation.before.path, err), rollbackErr)
		}
	}
	return nil
}

func rollbackStateMutations(mutations []Mutation) error {
	var rollbackErr error
	for i := len(mutations) - 1; i >= 0; i-- {
		snapshot := &mutations[i].before
		var err error
		if snapshot.exists {
			err = WriteFile(snapshot.path, snapshot.data, snapshot.mode)
		} else {
			err = os.Remove(snapshot.path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback %s: %w", snapshot.path, err))
		}
	}
	return rollbackErr
}
