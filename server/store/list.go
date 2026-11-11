package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// ListEntries returns the backend-neutral directory listing used by the
// server's DocumentStore contract. It applies the same filtering as ListDir.
func (s *Store) ListEntries(reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	entries, err := s.listDir(reqPath, opts)
	if err != nil {
		return nil, err
	}
	out := make([]storefmt.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, storefmt.DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

// listDir returns entries at reqPath, excluding dot-files, versions/, and
// non-documents (SPEC 9.8/11.9). includeArchived=false also omits archived
// docs and document-free subtrees; true is the recovery/audit view.
func (s *Store) listDir(reqPath string, opts storefmt.ListOptions) ([]os.DirEntry, error) {
	includeArchived := opts.IncludeArchived
	dirPath, err := s.resolve(reqPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(dirPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, os.ErrNotExist
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	// Dot-files, the versions directory and, unless asked for, archived
	// entries are filtered. liveChildren is one pathIdx pass: what the index
	// names as live never touches disk.
	absRoot, err := s.resolvedRoot()
	if err != nil {
		return nil, err
	}
	var live liveChildren
	if !includeArchived {
		live = s.liveChildren(reqPath)
	}
	dc := s.docCandidates(dirPath)
	filtered := entries[:0]
	// ReadDir sorts by name, so the window is a search and an early stop, both
	// ahead of the per entry disk checks.
	start := 0
	if opts.After != "" {
		start = sort.Search(len(entries), func(i int) bool { return entries[i].Name() > opts.After })
	}
	for _, e := range entries[start:] {
		if opts.Limit > 0 && len(filtered) == opts.Limit {
			break
		}
		name := e.Name()
		if isHiddenEntry(name) {
			continue
		}
		if e.IsDir() {
			var hasDocument bool
			if includeArchived {
				// Recovery/audit view: any document in the subtree, archived
				// included, keeps the directory visible.
				hasDocument, err = s.dirHasDocument(filepath.Join(dirPath, name), absRoot, true)
			} else {
				// Index fast path; on a miss, scan disk before pruning
				// rather than hide content the listing would show.
				if _, ok := live.dirs[name]; ok {
					hasDocument = true
				} else {
					hasDocument, err = s.dirHasDocument(filepath.Join(dirPath, name), absRoot, false)
				}
			}
			if err != nil {
				return nil, fmt.Errorf("inspect directory %q: %w", name, err)
			}
			if !hasDocument {
				continue
			}
			filtered = append(filtered, e)
			continue
		}
		// The index proves document identity, but archive state remains a disk
		// check so external sidecar corruption cannot fail open.
		_, indexed := live.files[name]
		if !indexed && !dc.isDocument(name) {
			continue
		}
		if !includeArchived {
			archived, err := entryArchived(filepath.Join(dirPath, name), absRoot)
			if err != nil {
				return nil, fmt.Errorf("inspect document %q: %w", name, err)
			}
			if archived {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	return filtered, nil
}

// docCandidates answers "is this file entry a served document?" (SPEC 9.8):
// its per-doc dir holds a version, so Get would serve it; a symlink earns no
// trust on its own (SPEC 6.2). Memoizes one versions/ ReadDir per listing.
type docCandidates struct {
	dirAbs string
	bases  map[string]bool
	loaded bool
}

func (s *Store) docCandidates(dirAbs string) *docCandidates {
	return &docCandidates{dirAbs: dirAbs}
}

func (dc *docCandidates) isDocument(name string) bool {
	if !dc.loaded {
		dc.loaded = true
		// A failed ReadDir leaves an empty set: an inaccessible versions/
		// hides its documents rather than serving them broken (same
		// trade-off as dirHasDocument and highestPerDocVersion).
		entries, err := os.ReadDir(filepath.Join(dc.dirAbs, versionsDirName))
		if err != nil {
			return false
		}
		dc.bases = make(map[string]bool, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				dc.bases[e.Name()] = true
			}
		}
	}
	return dc.bases[name] && highestPerDocVersion(filepath.Join(dc.dirAbs, versionsDirName), name) > 0
}

// isHiddenEntry reports whether a directory entry name is always excluded from
// a listing, regardless of archival: dot-files and the per-document versions/
// directory. Shared by ListDir so the exclusion list lives in one place.
func isHiddenEntry(name string) bool {
	return strings.HasPrefix(name, ".") || name == versionsDirName
}

// liveChildren names the immediate children of a listed directory that the
// index proves live: files is the live documents directly in the directory,
// dirs is the child directories holding a live document somewhere beneath.
type liveChildren struct {
	files map[string]struct{}
	dirs  map[string]struct{}
}

// liveChildren classifies the live documents under dirReq from pathIdx, so a
// LIST answers "is this file live?" and "does this child hold live docs?"
// without disk. Dot-named segments are skipped: a listing never shows them.
func (s *Store) liveChildren(dirReq string) liveChildren {
	canon := storefmt.CanonicalPath(dirReq)
	prefix := strings.TrimRight(canon, "/") + "/"
	out := liveChildren{files: make(map[string]struct{}), dirs: make(map[string]struct{})}
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	for p := range s.pathIdx {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok || hasHiddenSegment(rest) {
			continue
		}
		// A remaining slash means the first segment is a child directory
		// holding the doc; otherwise the doc is a file directly in dirReq.
		if child, _, isDir := strings.Cut(rest, "/"); isDir {
			out.dirs[child] = struct{}{}
		} else {
			out.files[rest] = struct{}{}
		}
	}
	return out
}

// hasHiddenSegment reports whether any segment of the relative slash path
// would be excluded from a listing by isHiddenEntry.
func hasHiddenSegment(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if isHiddenEntry(seg) {
			return true
		}
	}
	return false
}

// dirHasDocument reports whether the subtree holds a document the listing
// view would show (SPEC 11.9); liveChildren's slow-path complement, walked
// only for unindexed children. Unreadable dirs prune rather than show empty.
func (s *Store) dirHasDocument(dirAbs, absRoot string, includeArchived bool) (bool, error) {
	entries, err := os.ReadDir(dirAbs)
	if err != nil {
		return false, fmt.Errorf("read directory: %w", err)
	}
	// Files first: a qualifying file at this level answers with at most one
	// candidate load, before any subtree recursion is paid.
	dc := s.docCandidates(dirAbs)
	var dirs []string
	for _, e := range entries {
		name := e.Name()
		if isHiddenEntry(name) {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, name)
			continue
		}
		if !dc.isDocument(name) {
			continue
		}
		if includeArchived {
			return true, nil
		}
		archived, err := entryArchived(filepath.Join(dirAbs, name), absRoot)
		if err != nil {
			return false, err
		}
		if !archived {
			return true, nil
		}
	}
	for _, name := range dirs {
		hasDocument, err := s.dirHasDocument(filepath.Join(dirAbs, name), absRoot, includeArchived)
		if err != nil {
			return false, err
		}
		if hasDocument {
			return true, nil
		}
	}
	return false, nil
}

// entryArchived safely resolves a current document and overlays sidecar state.
// Confirmed non-symlinks and non-regular targets remain visible.
func entryArchived(childPath, absRoot string) (bool, error) {
	fi, err := os.Lstat(childPath)
	if err != nil {
		return false, fmt.Errorf("lstat document: %w", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	resolved, err := filepath.EvalSymlinks(childPath)
	if err != nil {
		return false, fmt.Errorf("resolve document: %w", err)
	}
	if !isContained(resolved, absRoot) {
		return false, errSymlinkEscapes
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return false, fmt.Errorf("stat document: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	// A sidecar decides archive state, so the header is read only without one.
	docDir := filepath.Dir(resolved)
	if archived, exists, err := readArchiveState(docDir); err != nil || exists {
		return archived, err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return false, fmt.Errorf("open document: %w", err)
	}
	// maxStoreFrontmatter bounds the frontmatter block, so this prefix is
	// guaranteed to contain the closing fence; the slack covers the fences.
	buf := make([]byte, storefmt.MaxStoreFrontmatter+256)
	n, readErr := io.ReadFull(f, buf)
	closeErr := f.Close()
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return false, fmt.Errorf("read current version: %w", errors.Join(readErr, closeErr))
	}
	if closeErr != nil {
		return false, fmt.Errorf("close current version: %w", closeErr)
	}
	return storefmt.IsArchived(buf[:n]), nil
}

// IsDir reports whether the given path is a directory within the content root.
func (s *Store) IsDir(reqPath string) (bool, error) {
	dirPath, err := s.resolve(reqPath)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(dirPath)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}
