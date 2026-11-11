package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// walkCurrentFiles calls fn with the raw stored bytes of each current, live
// document, under one set of skip and containment rules.
func (s *Store) walkCurrentFiles(fn func(reqPath string, data []byte, modified time.Time) error) error {
	absRoot, err := s.resolvedRoot()
	if err != nil {
		return err
	}

	// Entries that cannot be indexed are skipped, never fatal: one broken
	// symlink must not take the server down. They are reported as a
	// PartialWalkError so the caller can surface the degradation.
	partial := &storefmt.PartialWalkError{}
	record := func(path string, err error) {
		partial.Total++
		if len(partial.Skipped) < storefmt.MaxSkippedSample {
			partial.Skipped = append(partial.Skipped, storefmt.SkippedEntry{Path: path, Err: err})
		}
	}
	skip := func(path string, err error) error {
		record(path, err)
		return nil
	}
	walkErr := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == absRoot {
				// An unreadable root is not a partial walk: nothing was indexed.
				return fmt.Errorf("walk content root: %w", err)
			}
			return skip(path, err)
		}
		if d.IsDir() {
			if d.Name() == versionsDirName {
				// Legacy history is fatal: serving would hide those documents.
				// Any other read failure is one skipped directory, reported.
				if err := s.checkVersionsDir(path); errors.Is(err, ErrLegacyLayout) {
					return err
				} else if err != nil {
					record(path, err)
				}
				return filepath.SkipDir
			}
			return nil
		}
		// Only follow symlinks (current versions of documents).
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		// Resolve symlink and ensure target stays within the content root.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return skip(path, err)
		}
		if !isContained(resolved, absRoot) {
			return skip(path, errSymlinkEscapes)
		}
		// A document has a stored version in its per-doc dir (SPEC 11.9), the
		// same test Get applies; any other symlink is not a document.
		highest, err := docVersion(filepath.Join(filepath.Dir(path), versionsDirName), d.Name())
		if err != nil {
			return skip(path, err)
		}
		if highest == 0 {
			return nil
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return skip(path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > int64(protocol.MaxBodyLength+storefmt.MaxStoreFrontmatter) {
			return skip(path, storefmt.ErrSizeLimit)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return skip(path, err)
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return skip(path, err)
		}
		archived, err := effectiveArchived(filepath.Dir(resolved), data)
		if err != nil {
			return skip(path, err)
		}
		if archived {
			return nil
		}
		return fn("/"+filepath.ToSlash(rel), data, info.ModTime().UTC().Truncate(time.Second))
	})
	if walkErr != nil {
		return walkErr
	}
	if partial.Total > 0 {
		return partial
	}
	return nil
}

var errSymlinkEscapes = errors.New("current-version symlink escapes the content root")

// ErrLegacyLayout reports a pre-1.0 flat version layout (versions/doc.md.v1)
// that this release neither migrates nor serves.
var ErrLegacyLayout = errors.New("pre-1.0 flat version layout; migrate with a pre-1.0 demarkus release first")

// checkVersionsDir fails on a flat-layout version file; BuildIndex and write
// both run it, so nothing serves over or writes beside unmigrated history.
// A clean directory is remembered; a read failure is returned, never passed.
func (s *Store) checkVersionsDir(versionsDir string) error {
	s.cleanMu.Lock()
	_, clean := s.cleanDirs[versionsDir]
	s.cleanMu.Unlock()
	if clean {
		return nil
	}
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", versionsDir, err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() && isLegacyVersionName(e.Name()) {
			return fmt.Errorf("%s: %w", filepath.Join(versionsDir, e.Name()), ErrLegacyLayout)
		}
	}
	s.cleanMu.Lock()
	s.cleanDirs[versionsDir] = struct{}{}
	s.cleanMu.Unlock()
	return nil
}

// isLegacyVersionName matches the flat layout's "{base}.md.v{N}" filename;
// hidden bases never were documents.
func isLegacyVersionName(name string) bool {
	i := strings.LastIndex(name, ".md.v")
	if i <= 0 || isHiddenEntry(name[:i+3]) {
		return false
	}
	n, err := strconv.Atoi(name[i+5:])
	return err == nil && n >= 1
}

// BuildIndex walks the content root and indexes the current, live documents
// by path, with the version and body hash each folds into the fingerprint.
// Skips versions/ directories and archived documents.
func (s *Store) BuildIndex() error { return s.BuildIndexObserved(nil) }

// BuildIndexObserved is BuildIndex that also hands each indexed document to
// observe, so a catalog is built from the one walk. The index is built aside
// and swapped in whole: observe runs unlocked, a broken walk keeps the old.
func (s *Store) BuildIndexObserved(observe func(storefmt.CurrentDoc)) error {
	pathIdx := make(map[string][digestLen]byte)
	var digest [digestLen]byte
	err := s.walkCurrentFiles(func(reqPath string, data []byte, modified time.Time) error {
		body := storefmt.ExtractBody(data)
		if previous, ok := pathIdx[reqPath]; ok {
			xorInto(&digest, previous)
		}
		folded := fold(reqPath, indexVersion(data), storefmt.ContentHash(body))
		pathIdx[reqPath] = folded
		xorInto(&digest, folded)
		if observe != nil {
			observe(storefmt.CurrentDoc{Path: reqPath, Body: body, Metadata: storefmt.ExtractMetadata(data), Modified: modified})
		}
		return nil
	})
	var partial *storefmt.PartialWalkError
	if err != nil && !errors.As(err, &partial) {
		return err
	}
	s.indexMu.Lock()
	s.pathIdx, s.digest, s.indexed = pathIdx, digest, true
	s.indexMu.Unlock()
	return err
}
