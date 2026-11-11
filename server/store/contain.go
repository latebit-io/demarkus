package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// resolvedRoot returns the absolute, symlink-resolved path for the content root.
func (s *Store) resolvedRoot() (string, error) {
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root symlinks: %w", err)
	}
	return resolved, nil
}

// isContained reports whether absPath is equal to or beneath absRoot.
func isContained(absPath, absRoot string) bool {
	return absPath == absRoot || strings.HasPrefix(absPath, absRoot+string(filepath.Separator))
}

// resolve validates and resolves a request path to an absolute filesystem path
// within the content directory. Returns os.ErrNotExist for invalid paths.
func (s *Store) resolve(reqPath string) (string, error) {
	// RelPath refuses ".." before cleaning, which would otherwise turn
	// /../etc/passwd into a valid looking etc/passwd.
	rel, err := storefmt.RelPath(reqPath)
	if err != nil {
		return "", err
	}
	return s.contain(filepath.Join(s.root, filepath.FromSlash(rel)))
}

// containWrite keeps a write inside the root: the document path and its
// versions tree, which can be a planted symlink even when the path is not.
func (s *Store) containWrite(reqPath string, loc *docLocation) error {
	if _, err := s.resolve(reqPath); err != nil {
		if errors.Is(err, errUnderDocument) {
			// Topology collision, same class as writing over a directory.
			return fmt.Errorf("cannot publish %s: a document exists at an ancestor: %w", reqPath, storefmt.ErrPathCollision)
		}
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("resolve path: %w", err)
	}
	if _, err := s.contain(loc.docDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("resolve version tree: %w", err)
	}
	return nil
}

// contain resolves symlinks in a path under the root and returns os.ErrNotExist
// when the result escapes it. The path need not exist yet.
func (s *Store) contain(joined string) (string, error) {
	absRoot, err := s.resolvedRoot()
	if err != nil {
		return "", err
	}

	absPath, err := filepath.EvalSymlinks(joined)
	if err != nil {
		// Not there yet: resolve the closest existing ancestor and append the
		// rest, so macOS's /var → /private/var and a symlinked intermediate
		// directory are both seen.
		absPath, err = resolveNonExistent(joined)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(absPath) {
		absPath, err = filepath.Abs(absPath)
		if err != nil {
			return "", err
		}
	}

	if !isContained(absPath, absRoot) {
		return "", os.ErrNotExist
	}
	return absPath, nil
}

// resolveNonExistent resolves a path that does not exist yet: the closest
// existing ancestor with its symlinks resolved, then the remaining segments,
// so an escape through an intermediate directory is detected.
func resolveNonExistent(path string) (string, error) {
	var tail []string
	current := path
	for {
		if _, err := os.Lstat(current); err == nil {
			break
		}
		tail = append([]string{filepath.Base(current)}, tail...)
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root without finding an existing dir.
			return filepath.Abs(path)
		}
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return filepath.Abs(path)
	}
	// A non-directory ancestor (a document's current pointer) cannot have
	// children; surface that as ErrNotExist rather than a later ENOTDIR.
	if len(tail) > 0 {
		if info, err := os.Stat(resolved); err == nil && !info.IsDir() {
			return "", errUnderDocument
		}
	}
	return filepath.Join(append([]string{resolved}, tail...)...), nil
}

// errUnderDocument marks a path beneath a document. Readers see ErrNotExist;
// write maps it to the topology collision error.
var errUnderDocument = fmt.Errorf("%w: path is beneath a document", os.ErrNotExist)
