// Package store provides versioned document storage for the Demarkus server.
//
// The store manages documents in a content directory. Only documents written
// through the protocol (with a versions directory) are served. Flat files
// without version history are not documents (SPEC 9.8, 11.9): FETCH treats
// them as non-existent, LIST omits them, and a publish to their path replaces
// them without incorporating their content.
//
// Layout (per-document subdirectory):
//
//	root/
//	  doc.md              ← symlink to versions/doc.md/v3
//	  versions/
//	    doc.md/
//	      v1
//	      v2
//	      v3
//
// A legacy flat layout (versions/doc.md.v{N}) predates the per-document
// subdirectory layout. The read and write paths assume per-doc layout only;
// callers opening a store over a pre-existing root run MigrateLegacyLayout
// once at startup to convert any remaining legacy files.
// TODO(v1): remove MigrateLegacyLayout once no pre-per-doc stores remain.
package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

const archiveStateName = "archive-state"

// newVersionFilePath returns the absolute path for a new version file,
// always using the per-document subdirectory layout.
func newVersionFilePath(versionsDir, base string, version int) string {
	return filepath.Join(versionsDir, base, fmt.Sprintf("v%d", version))
}

func readArchiveState(docDir string) (archived, exists bool, err error) {
	statePath := filepath.Join(docDir, archiveStateName)
	info, err := os.Lstat(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("stat archive state: %w", err)
	}
	if !info.Mode().IsRegular() || (info.Size() != int64(len("true\n")) && info.Size() != int64(len("false\n"))) {
		return false, false, fmt.Errorf("%w: invalid archive state file", storefmt.ErrIntegrity)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return false, false, fmt.Errorf("read archive state: %w", err)
	}
	switch string(data) {
	case "true\n":
		return true, true, nil
	case "false\n":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("%w: invalid archive state %q", storefmt.ErrIntegrity, data)
	}
}

func effectiveArchived(docDir string, tipStored []byte) (bool, error) {
	archived, exists, err := readArchiveState(docDir)
	if err != nil {
		return false, err
	}
	if exists {
		return archived, nil
	}
	return storefmt.IsArchived(tipStored), nil
}

// writeArchiveState commits through a synced sibling temp file. committed is
// true when rename succeeded, even if the following directory sync failed.
func writeArchiveState(docDir string, archived bool) (committed bool, retErr error) {
	f, err := os.CreateTemp(docDir, ".archive-state-*")
	if err != nil {
		return false, fmt.Errorf("create temp archive state: %w", err)
	}
	tmpPath := f.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := f.Close(); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close temp archive state: %w", closeErr))
			}
		}
		if tmpPath != "" {
			if removeErr := os.Remove(tmpPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("remove temp archive state: %w", removeErr))
			}
		}
	}()

	if err := f.Chmod(0o644); err != nil {
		return false, fmt.Errorf("chmod temp archive state: %w", err)
	}
	state := strconv.FormatBool(archived) + "\n"
	if _, err := f.WriteString(state); err != nil {
		return false, fmt.Errorf("write temp archive state: %w", err)
	}
	if err := f.Sync(); err != nil {
		return false, fmt.Errorf("sync temp archive state: %w", err)
	}
	if err := f.Close(); err != nil {
		closed = true
		return false, fmt.Errorf("close temp archive state: %w", err)
	}
	closed = true

	statePath := filepath.Join(docDir, archiveStateName)
	if err := os.Rename(tmpPath, statePath); err != nil {
		return false, fmt.Errorf("rename archive state: %w", err)
	}
	tmpPath = ""
	committed = true

	if err := syncArchiveStateDir(docDir); err != nil {
		return true, err
	}
	return true, nil
}

func syncArchiveStateDir(docDir string) error {
	dir, err := os.Open(docDir)
	if err != nil {
		return fmt.Errorf("open archive state directory: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(
			wrapError("sync archive state directory", syncErr),
			wrapError("close archive state directory", closeErr),
		)
	}
	return nil
}

func wrapError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

// newVersionSymlinkTarget returns the relative symlink target for a new version,
// always using the per-document subdirectory layout.
func newVersionSymlinkTarget(base string, version int) string {
	return filepath.Join(versionsDirName, base, fmt.Sprintf("v%d", version))
}

// Store provides read access to a versioned document directory.
type Store struct {
	root   string
	hashMu sync.RWMutex
	// hashIdx: content hash → sorted canonical request paths whose current
	// body has that hash. LookupHash answers the smallest so any backend and
	// a rebuilt index agree on shared bodies.
	hashIdx map[string][]string
	// pathIdx: canonical request path → content hash. liveChildren also reads
	// membership as "current, non-archived doc" for ListDir filtering, so
	// keep membership semantics exact; a miss only degrades to a disk scan.
	pathIdx map[string]string
	// hashErr records an incomplete index build. Hits remain valid, but misses
	// cannot be reported as confirmed absence until a clean rebuild succeeds.
	hashErr error
	dirDebt dirSyncDebt
}

// New creates a store rooted at the given directory. For a pre-existing root
// use Open, which migrates any legacy-layout version files first.
func New(root string) *Store {
	return &Store{
		root:    root,
		hashIdx: make(map[string][]string),
		pathIdx: make(map[string]string),
	}
}

// Open returns a store for an existing root, migrating legacy flat-layout
// versions first: an unmigrated root would hide every legacy document and
// restart its numbering. TODO(v1): fold into New with migrateLegacyLayout.
func Open(root string) (*Store, error) {
	s := New(root)
	if err := s.migrateLegacyLayout(); err != nil {
		return nil, fmt.Errorf("migrate legacy layout: %w", err)
	}
	return s, nil
}

// walkCurrentFiles calls fn with the raw stored bytes of each current, live
// document. BuildHashIndex and WalkCurrent share it so the skip and containment
// rules live in one place.
func (s *Store) walkCurrentFiles(fn func(reqPath string, data []byte, modified time.Time) error) error {
	absRoot, err := s.resolvedRoot()
	if err != nil {
		return err
	}

	// Entries that cannot be indexed are skipped, never fatal: one broken
	// symlink must not take the server down. They are reported as a
	// PartialWalkError so the caller can surface the degradation.
	partial := &storefmt.PartialWalkError{}
	skip := func(path string, err error) error {
		partial.Total++
		if len(partial.Skipped) < storefmt.MaxSkippedSample {
			partial.Skipped = append(partial.Skipped, storefmt.SkippedEntry{Path: path, Err: err})
		}
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
		info, err := os.Stat(resolved)
		if err != nil {
			return skip(path, err)
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

// BuildHashIndex walks the content root and indexes current versions by content hash.
// Skips versions/ directories and archived documents.
func (s *Store) BuildHashIndex() error {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()

	s.hashIdx = make(map[string][]string)
	s.pathIdx = make(map[string]string)
	s.hashErr = nil

	err := s.walkCurrentFiles(func(reqPath string, data []byte, _ time.Time) error {
		s.indexLocked(reqPath, storefmt.ContentHash(storefmt.ExtractBody(data)))
		return nil
	})
	s.hashErr = err
	return err
}

// indexLocked records reqPath (already canonical) under hash, dropping any
// earlier hash it was indexed under. Caller holds hashMu.
func (s *Store) indexLocked(reqPath, hash string) {
	s.unindexLocked(reqPath)
	paths := s.hashIdx[hash]
	i, _ := slices.BinarySearch(paths, reqPath)
	s.hashIdx[hash] = slices.Insert(paths, i, reqPath)
	s.pathIdx[reqPath] = hash
}

// unindexLocked removes reqPath from both maps. Caller holds hashMu.
func (s *Store) unindexLocked(reqPath string) {
	hash, ok := s.pathIdx[reqPath]
	if !ok {
		return
	}
	delete(s.pathIdx, reqPath)
	paths := s.hashIdx[hash]
	if i, found := slices.BinarySearch(paths, reqPath); found {
		paths = slices.Delete(paths, i, i+1)
	}
	if len(paths) == 0 {
		delete(s.hashIdx, hash)
	} else {
		s.hashIdx[hash] = paths
	}
}

// WalkCurrent visits every current, non-archived document, with the same skip
// rules as BuildHashIndex, so a derived index reads the same source of truth.
func (s *Store) WalkCurrent(fn func(storefmt.CurrentDoc) error) error {
	return s.walkCurrentFiles(func(reqPath string, data []byte, modified time.Time) error {
		return fn(storefmt.CurrentDoc{
			Path:     reqPath,
			Body:     storefmt.ExtractBody(data),
			Metadata: storefmt.ExtractMetadata(data),
			Modified: modified,
		})
	})
}

// LookupHashResult returns the smallest live request path for a content hash.
// A confirmed miss is os.ErrNotExist; an incomplete index reports its cause.
func (s *Store) LookupHashResult(hash string) (string, error) {
	s.hashMu.RLock()
	defer s.hashMu.RUnlock()
	if paths := s.hashIdx[hash]; len(paths) > 0 {
		return paths[0], nil
	}
	if s.hashErr != nil {
		return "", fmt.Errorf("hash index incomplete: %w", s.hashErr)
	}
	return "", os.ErrNotExist
}

// UpdateHashIndex adds or updates the hash index entry for a document.
// Keys are canonical so "/d/e/" and "/d/e" index one document, as
// BuildHashIndex derives from the walk.
func (s *Store) UpdateHashIndex(reqPath string, body []byte) {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	s.indexLocked(storefmt.CanonicalPath(reqPath), storefmt.ContentHash(body))
}

// RemoveHashEntry removes the hash index entry for a given request path.
func (s *Store) RemoveHashEntry(reqPath string) {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	s.unindexLocked(storefmt.CanonicalPath(reqPath))
}

// HashIndexSize returns the number of indexed documents.
func (s *Store) HashIndexSize() int {
	s.hashMu.RLock()
	defer s.hashMu.RUnlock()
	return len(s.pathIdx)
}

// Root returns the content directory path.
func (s *Store) Root() string {
	return s.root
}

// VersionFilePath returns where version N of reqPath lives on disk, for
// tools and tests that touch the layout directly. It does not check that
// the file exists or follow symlinks.
func (s *Store) VersionFilePath(reqPath string, version int) (string, error) {
	loc, err := s.locateContained(reqPath)
	if err != nil {
		return "", err
	}
	return loc.versionFile(version), nil
}

// Get retrieves a document at the given path. If version is 0, returns the
// current version. Only serves documents with a versions directory — flat files
// without version history are treated as non-existent.
func (s *Store) Get(reqPath string, version int) (*storefmt.Document, error) {
	if version > 0 {
		return s.getVersion(reqPath, version)
	}

	filePath, err := s.resolve(reqPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, os.ErrNotExist
	}
	if info.Size() > int64(protocol.MaxBodyLength+storefmt.MaxStoreFrontmatter) {
		return nil, fmt.Errorf("file exceeds size limit")
	}

	// Only serve documents written through the protocol (with version history).
	versions := s.findVersions(reqPath)
	if len(versions) == 0 {
		return nil, os.ErrNotExist
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	archived, err := effectiveArchived(filepath.Dir(filePath), data)
	if err != nil {
		return nil, err
	}

	ver, err := s.CurrentVersionResult(reqPath)
	if err != nil {
		return nil, err
	}

	return &storefmt.Document{
		Content:  storefmt.ExtractBody(data),
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  ver,
		Archived: archived,
		Metadata: storefmt.ExtractMetadata(data),
		ETag:     storefmt.StoredETag(data),
	}, nil
}

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

	// Filter dot-files and the versions directory, plus archived entries
	// unless the caller asked to see them. liveChildren is computed once (a
	// single pathIdx pass) so classification is an O(1) lookup per entry —
	// files and directories the index names as live never touch disk.
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

// liveChildren makes a single pass over the in-memory pathIdx — which the
// store maintains incrementally on every write and archive/unarchive (see
// UpdateHashIndex / RemoveHashEntry) — and classifies the live docs under the
// directory request path dirReq, so a LIST answers both "is this file live?"
// and "does this child directory hold live docs?" with O(1) lookups, never
// touching disk for indexed entries.
//
// The request path is canonicalized first: pathIdx keys are canonical
// ("/"+filepath.Rel), so a non-canonical caller path ("//docs", "/docs/.")
// would otherwise match nothing and silently force the disk fallback for
// every child. Docs with a hidden path segment (dot-named) are skipped even
// though the index tracks them: a listing never shows them, so counting one
// as live would keep its parent visible as an empty shell.
func (s *Store) liveChildren(dirReq string) liveChildren {
	canon := storefmt.CanonicalPath(dirReq)
	prefix := strings.TrimRight(canon, "/") + "/"
	out := liveChildren{files: make(map[string]struct{}), dirs: make(map[string]struct{})}
	s.hashMu.RLock()
	defer s.hashMu.RUnlock()
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
	return effectiveArchived(filepath.Dir(resolved), buf[:n])
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

// Versions returns the version history for a document, newest first.
// Returns os.ErrNotExist if the document has no version history.
func (s *Store) Versions(reqPath string) ([]storefmt.VersionInfo, error) {
	filePath, err := s.resolve(reqPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, os.ErrNotExist
	}

	versions := s.findVersions(reqPath)
	if len(versions) == 0 {
		return nil, os.ErrNotExist
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version > versions[j].Version
	})
	return versions, nil
}

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
		// Path doesn't exist yet — walk up to find the closest existing
		// ancestor, resolve its symlinks, then append the remaining segments.
		// This prevents both the /var → /private/var mismatch on macOS and
		// symlink escapes through intermediate directories.
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

// CurrentVersionResult returns the latest version number and path errors.
func (s *Store) CurrentVersionResult(reqPath string) (int, error) {
	loc, err := s.locate(reqPath)
	if err != nil {
		return 0, err
	}
	return s.currentVersion(&loc)
}

// currentVersion is CurrentVersionResult for an already derived location.
func (s *Store) currentVersion(loc *docLocation) (int, error) {
	if _, err := s.contain(loc.docDir); err != nil {
		// Escaping or beneath a document: no versions here, not a path error.
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return highestPerDocVersion(loc.versionsDir, loc.base), nil
}

// highestPerDocVersion returns the highest version in base's per-doc dir,
// or 0 (names-only, no stats). An unreadable dir is also 0: an inaccessible
// doc hides rather than serves broken, and heals with the permission.
func highestPerDocVersion(versionsDir, base string) int {
	entries, err := os.ReadDir(filepath.Join(versionsDir, base))
	if err != nil {
		return 0
	}
	highest := 0
	for _, e := range entries {
		if n := perDocVersionNumber(e); n > highest {
			highest = n
		}
	}
	return highest
}

// perDocVersionNumber parses a per-doc version filename ("v{N}", N >= 1),
// 0 otherwise. Regular files only: a planted symlink is never a version,
// anywhere the store answers version questions.
func perDocVersionNumber(e os.DirEntry) int {
	if !e.Type().IsRegular() {
		return 0
	}
	name := e.Name()
	if !strings.HasPrefix(name, "v") {
		return 0
	}
	n, err := strconv.Atoi(name[1:])
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// findVersions looks for versioned files in the versions directory
// (per-document subdirectory layout, versions/{base}/v{N}).
// Returns nil if no versions directory or no matching files exist.
func (s *Store) findVersions(reqPath string) []storefmt.VersionInfo {
	// An escaping document has no versions here; readers see it as absent.
	loc, err := s.locateContained(reqPath)
	if err != nil {
		return nil
	}
	return s.findVersionsPerDoc(loc.versionsDir, loc.base)
}

// findVersionsPerDoc reads versions from the per-document subdirectory layout.
func (s *Store) findVersionsPerDoc(versionsDir, base string) []storefmt.VersionInfo {
	docDir := filepath.Join(versionsDir, base)
	entries, err := os.ReadDir(docDir)
	if err != nil {
		return nil
	}

	var versions []storefmt.VersionInfo
	for _, e := range entries {
		num := perDocVersionNumber(e)
		if num == 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		versions = append(versions, storefmt.VersionInfo{
			Version:  num,
			Modified: info.ModTime().UTC().Truncate(time.Second),
		})
	}
	return versions
}

// getVersion retrieves a specific version of a document from the versions directory.
// Uses resolve() for path validation — same security as all other path access.
func (s *Store) getVersion(reqPath string, version int) (*storefmt.Document, error) {
	loc, err := s.locate(reqPath)
	if err != nil {
		return nil, err
	}
	filePath, err := s.resolve(loc.versionReqPath(version))
	if err != nil {
		return nil, err
	}

	// Lstat the unresolved name: a planted symlink is never a version.
	info, err := os.Lstat(loc.versionFile(version))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	if info.Size() > int64(protocol.MaxBodyLength+storefmt.MaxStoreFrontmatter) {
		return nil, fmt.Errorf("file exceeds size limit")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	return &storefmt.Document{
		Content:  storefmt.ExtractBody(data),
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  version,
		Archived: false,
		Metadata: storefmt.ExtractMetadata(data),
		ETag:     storefmt.StoredETag(data),
	}, nil
}

// ArchiveResult changes archive state and returns its committed result.
func (s *Store) ArchiveResult(reqPath string, archived bool) (*storefmt.Document, bool, error) {
	return s.ArchiveChecked(&storefmt.ArchiveSpec{ArchiveChange: storefmt.ArchiveChange{Path: reqPath, Archived: archived}})
}

// ArchiveChecked is ArchiveResult with an optional check run before the state
// changes; a no-op transition is decided first and never reaches it.
func (s *Store) ArchiveChecked(spec *storefmt.ArchiveSpec) (*storefmt.Document, bool, error) {
	reqPath, archived := spec.Path, spec.Archived
	if _, err := s.resolve(reqPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, os.ErrNotExist
		}
		return nil, false, fmt.Errorf("resolve path: %w", err)
	}

	loc, err := s.locate(reqPath)
	if err != nil {
		return nil, false, err
	}
	currentVersion, err := s.currentVersion(&loc)
	if err != nil {
		return nil, false, err
	}
	if currentVersion == 0 {
		return nil, false, os.ErrNotExist
	}

	versionFile, err := s.resolve(loc.versionReqPath(currentVersion))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, os.ErrNotExist
		}
		return nil, false, fmt.Errorf("resolve version file: %w", err)
	}

	data, err := os.ReadFile(versionFile)
	if err != nil {
		return nil, false, fmt.Errorf("read version file: %w", err)
	}
	info, err := os.Stat(versionFile)
	if err != nil {
		return nil, false, fmt.Errorf("stat version file: %w", err)
	}
	currentArchived, err := effectiveArchived(filepath.Dir(versionFile), data)
	if err != nil {
		return nil, false, fmt.Errorf("read current archive state: %w", err)
	}
	doc := documentFromStored(data, info.ModTime(), currentVersion, archived)
	if currentArchived == archived {
		if _, err := os.Stat(filepath.Join(filepath.Dir(versionFile), archiveStateName)); err == nil {
			if err := syncArchiveStateDir(filepath.Dir(versionFile)); err != nil {
				return doc, false, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return doc, false, fmt.Errorf("stat archive state: %w", err)
		}
		s.updateArchiveIndex(reqPath, data, archived)
		return doc, false, nil
	}
	if spec.Check != nil {
		if err := spec.Check(storefmt.ArchiveChange{Path: storefmt.CanonicalPath(reqPath), Archived: archived}); err != nil {
			return nil, false, err
		}
	}

	committed, stateErr := writeArchiveState(filepath.Dir(versionFile), archived)
	if !committed {
		return nil, false, stateErr
	}

	s.updateArchiveIndex(reqPath, data, archived)

	if stateErr != nil {
		return doc, true, stateErr
	}
	return doc, true, nil
}

func (s *Store) updateArchiveIndex(reqPath string, stored []byte, archived bool) {
	if archived {
		s.RemoveHashEntry(reqPath)
		return
	}
	s.UpdateHashIndex(reqPath, storefmt.ExtractBody(stored))
}

func documentFromStored(data []byte, modified time.Time, version int, archived bool) *storefmt.Document {
	return &storefmt.Document{
		Content:  storefmt.ExtractBody(data),
		Modified: modified.UTC().Truncate(time.Second),
		Version:  version,
		Archived: archived,
		Metadata: storefmt.ExtractMetadata(data),
		ETag:     storefmt.StoredETag(data),
	}
}

// Write creates a new version of a document. Every call produces a new
// immutable version file; existing versions are never modified.
//
// The stored file is prefixed with a store-managed frontmatter block:
//
//	---
//	version: N
//	previous-hash: sha256-<hex>   ← omitted for v1
//	archived: true|false
//	tags: [a, b]                  ← recognized OKF fields, written bare
//	meta.key: value               ← other publisher metadata (0–10 keys total)
//	---
//	<original content>
//
// The previous-hash is the SHA-256 of the raw on-disk bytes of version N-1,
// forming a hash chain that allows chain integrity to be verified later.
func (s *Store) Write(reqPath string, content []byte, meta map[string]string) (*storefmt.Document, error) {
	if err := storefmt.ValidateWrite(content, meta); err != nil {
		return nil, err
	}
	return s.write(reqPath, content, meta, nil)
}

// syncFile and syncDir are the durability calls; tests replace them.
var (
	syncFile = (*os.File).Sync
	syncDir  = syncDirectory
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

// createVersionFile writes and syncs a new version. O_EXCL is the immutability
// guard: an existing file fails the create instead of racing a stat.
func createVersionFile(vFile string, stored []byte, version int) error {
	f, err := os.OpenFile(vFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("version %d: %w", version, storefmt.ErrVersionExists)
		}
		return fmt.Errorf("create version file: %w", err)
	}
	if _, err := f.Write(stored); err != nil {
		return errors.Join(fmt.Errorf("write version file: %w", err), f.Close(), removeIfPresent(vFile))
	}
	if err := syncFile(f); err != nil {
		return errors.Join(fmt.Errorf("sync version file: %w", err), f.Close(), removeIfPresent(vFile))
	}
	if err := f.Close(); err != nil {
		return errors.Join(fmt.Errorf("close version file: %w", err), removeIfPresent(vFile))
	}
	if err := syncDir(filepath.Dir(vFile)); err != nil {
		return errors.Join(err, removeIfPresent(vFile))
	}
	return nil
}

// errSwapNotApplied marks a swap that failed before the pointer moved, so the
// new version file is unreferenced and safe to remove.
var errSwapNotApplied = errors.New("current pointer not updated")

// swapCurrent points the current file at relTarget by renaming a temp symlink
// over it, so readers never see it missing. Relative, so the root can move.
func swapCurrent(currentFile, relTarget string) error {
	tmpLink := currentFile + ".tmp"
	if err := removeIfPresent(tmpLink); err != nil {
		return fmt.Errorf("clear stale temp link: %w: %w", errSwapNotApplied, err)
	}
	if err := os.Symlink(relTarget, tmpLink); err != nil {
		return fmt.Errorf("symlink current file: %w: %w", errSwapNotApplied, err)
	}
	if err := os.Rename(tmpLink, currentFile); err != nil {
		return errors.Join(fmt.Errorf("rename current file: %w: %w", errSwapNotApplied, err), removeIfPresent(tmpLink))
	}
	return syncDir(filepath.Dir(currentFile))
}

func removeIfPresent(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// write is the validated core shared by Write and WriteVersion.
func (s *Store) write(reqPath string, content []byte, meta map[string]string, check storefmt.WriteCheck) (*storefmt.Document, error) {
	loc, err := s.locate(reqPath)
	if err != nil {
		return nil, err
	}
	if err := s.containWrite(reqPath, &loc); err != nil {
		return nil, err
	}
	base, versionsDir, currentFile := loc.base, loc.versionsDir, loc.current

	if err := s.mkdirAllDurable(versionsDir); err != nil {
		return nil, fmt.Errorf("create versions dir: %w", err)
	}

	// Determine the next version number. For a truly new document (no current
	// file on disk), start at 1. Otherwise increment from the current version.
	var next int
	if info, err := os.Stat(currentFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			next = 1
		} else {
			return nil, fmt.Errorf("stat current file: %w", err)
		}
	} else if info.IsDir() {
		// Reject the document/directory collision before any version file is
		// written; failing later on the current-pointer rename would leave a
		// dangling version file behind.
		return nil, fmt.Errorf("cannot publish %s: a directory exists at this path: %w", reqPath, storefmt.ErrPathCollision)
	} else {
		// containWrite already held the versions tree inside the root.
		next = highestPerDocVersion(versionsDir, base) + 1
	}

	// For existing documents: reject writes to archived documents (closes
	// TOCTOU gap with handler) and check for duplicate content.
	if next > 1 {
		doc, err := s.prepareExistingDoc(versionsDir, base, next, content, meta)
		if doc != nil || err != nil {
			return doc, err
		}
	}

	vFile := loc.versionFile(next)

	stored, err := buildVersionFile(versionsDir, base, next, content, meta)
	if err != nil {
		return nil, err
	}

	// Validate stored size after prepending frontmatter.
	if int64(len(stored)) > int64(protocol.MaxBodyLength+storefmt.MaxStoreFrontmatter) {
		return nil, storefmt.ErrSizeLimit
	}
	// The persisted form, not the request map: tags are normalized on the way in.
	persisted := storefmt.ExtractMetadata(stored)
	if check != nil {
		prepared := storefmt.PreparedWrite{Path: storefmt.CanonicalPath(reqPath), Content: content, Metadata: persisted}
		if err := check(prepared); err != nil {
			return nil, err
		}
	}

	// Create per-document subdirectory for new documents.
	docDir := loc.docDir
	if err := s.mkdirAllDurable(docDir); err != nil {
		return nil, fmt.Errorf("create per-doc versions dir: %w", err)
	}

	if err := createVersionFile(vFile, stored, next); err != nil {
		return nil, err
	}
	if err := swapCurrent(currentFile, newVersionSymlinkTarget(base, next)); err != nil {
		// An unreferenced version would fail every later write with O_EXCL.
		if errors.Is(err, errSwapNotApplied) {
			err = errors.Join(err, removeIfPresent(vFile))
		}
		return nil, err
	}

	info, err := os.Stat(vFile)
	if err != nil {
		return nil, fmt.Errorf("stat version file: %w", err)
	}

	s.UpdateHashIndex(reqPath, content)

	// Metadata is the persisted form (what Get returns), not the request
	// map: "alpha, beta" is stored as a tags list and reads back "alpha,beta",
	// and the catalog must see one spelling whichever path populated it.
	doc := &storefmt.Document{
		Content:  content,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  next,
		Archived: false,
		Metadata: persisted,
		ETag:     storefmt.StoredETag(stored),
	}
	if keep := storefmt.RetentionValue(meta); keep > 0 {
		doc.Prune = s.pruneVersions(versionsDir, base, next, keep)
	}
	return doc, nil
}

// pruneVersions keeps the newest keep versions, deleting oldest first and
// stopping at the first failure so survivors stay a contiguous suffix. Removals
// go through an os.Root at the store root; a planted symlink cannot redirect one.
func (s *Store) pruneVersions(versionsDir, base string, current, keep int) *storefmt.PruneResult {
	// Unbounded on purpose: the first retained write prunes the whole backlog.
	cutoff := current - keep // delete versions <= cutoff
	if cutoff < 1 {
		return nil
	}
	relDocDir, err := filepath.Rel(s.root, filepath.Join(versionsDir, base))
	if err != nil || !filepath.IsLocal(relDocDir) {
		return &storefmt.PruneResult{Err: fmt.Errorf("prune: doc dir escapes store root: %q", relDocDir)}
	}
	rootFS, err := os.OpenRoot(s.root)
	if err != nil {
		return &storefmt.PruneResult{Err: fmt.Errorf("prune: open store root: %w", err)}
	}
	// Read-only directory handle; nothing actionable on close failure.
	defer func() { _ = rootFS.Close() }()

	versions := s.findVersionsPerDoc(versionsDir, base)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version < versions[j].Version
	})
	var res *storefmt.PruneResult
	for _, v := range versions {
		if v.Version > cutoff {
			break
		}
		if err := rootFS.Remove(filepath.Join(relDocDir, fmt.Sprintf("v%d", v.Version))); err != nil {
			if res == nil {
				res = &storefmt.PruneResult{}
			}
			res.Err = fmt.Errorf("prune v%d: %w", v.Version, err)
			break
		}
		if res == nil {
			res = &storefmt.PruneResult{From: v.Version}
		}
		res.To = v.Version
	}
	return res
}

// prepareExistingDoc rejects archived documents and returns ErrNotModified
// on unchanged content; (nil, nil) means proceed with a new version.
func (s *Store) prepareExistingDoc(versionsDir, base string, next int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	archived, err := s.isCurrentArchived(versionsDir, base, next-1)
	if err != nil {
		return nil, err
	}
	if archived {
		return nil, storefmt.ErrArchived
	}

	// Skip creating a new version if content and metadata are identical.
	prevFile := newVersionFilePath(versionsDir, base, next-1)
	prevData, err := os.ReadFile(prevFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read previous version: %w", err)
		}
		// File doesn't exist — proceed with writing the new version.
		return nil, nil
	}
	storedMeta := storefmt.ExtractMetadata(prevData)
	if bytes.Equal(storefmt.ExtractBody(prevData), content) && storefmt.MetaEqual(storedMeta, storefmt.NormalizeMetadata(meta)) {
		info, err := os.Stat(prevFile)
		if err != nil {
			return nil, fmt.Errorf("stat current version: %w", err)
		}
		return &storefmt.Document{
			Content:  content,
			Modified: info.ModTime().UTC().Truncate(time.Second),
			Version:  next - 1,
			Archived: false,
			Metadata: storedMeta,
			ETag:     storefmt.StoredETag(prevData),
		}, storefmt.ErrNotModified
	}
	return nil, nil
}

// WriteVersion is like Write but performs an optimistic concurrency check.
// expectedVersion semantics:
//   - < 0: skip check (equivalent to calling Write directly)
//   - 0: expect the document does not exist yet (create-only)
//   - > 0: expect this specific version (update-only)
//
// Returns ErrConflict if the expectation is violated.
func (s *Store) WriteVersion(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	return s.WriteChecked(&storefmt.WriteSpec{Path: reqPath, ExpectedVersion: expectedVersion, Content: content, Metadata: meta})
}

// WriteChecked is WriteVersion with an optional check run inside the write.
func (s *Store) WriteChecked(spec *storefmt.WriteSpec) (*storefmt.Document, error) {
	reqPath, expectedVersion, content, meta := spec.Path, spec.ExpectedVersion, spec.Content, spec.Metadata
	// Request-shaped checks come before any state read so an invalid write
	// never masquerades as a conflict; every backend must order checks this way.
	if err := storefmt.ValidateWrite(content, meta); err != nil {
		return nil, err
	}
	if storefmt.ContainsDotDot(reqPath) {
		return nil, os.ErrNotExist
	}
	if expectedVersion < 0 {
		return s.write(reqPath, content, meta, spec.Check)
	}

	current, err := s.CurrentVersionResult(reqPath)
	if err != nil {
		return nil, err
	}
	if current != expectedVersion {
		return &storefmt.Document{Version: current}, storefmt.ErrConflict
	}

	doc, err := s.write(reqPath, content, meta, spec.Check)
	if err != nil {
		if errors.Is(err, storefmt.ErrVersionExists) {
			// Lost the O_EXCL race: another writer created the expected
			// next version between our check and the file create.
			current, currentErr := s.CurrentVersionResult(reqPath)
			if currentErr != nil {
				return nil, currentErr
			}
			return &storefmt.Document{Version: current}, storefmt.ErrConflict
		}
		if errors.Is(err, storefmt.ErrNotModified) && doc != nil {
			// Content matches current version — but if a concurrent writer
			// created intervening versions, the "current" version may have
			// moved past expectedVersion+1. Allow not-modified only when
			// the version is what we'd expect.
			if doc.Version != expectedVersion && doc.Version != expectedVersion+1 {
				return &storefmt.Document{Version: doc.Version}, storefmt.ErrConflict
			}
		}
		return doc, err
	}

	// Post-check: if a concurrent writer slipped in between our pre-check
	// and Write's internal version computation, Write may have created a
	// version beyond expectedVersion+1 (e.g. v3 instead of v2). Detect
	// this and treat it as a conflict. The written version file is kept
	// to avoid leaving a dangling symlink — it's a valid version with a
	// correct hash chain, just created under stale assumptions. The prune
	// result is preserved: the write pruned regardless of the conflict,
	// and callers must still be able to audit-log the deletion.
	if doc.Version != expectedVersion+1 {
		return &storefmt.Document{Version: doc.Version, Prune: doc.Prune}, storefmt.ErrConflict
	}

	return doc, nil
}

// Append reads the document at expectedVersion, appends content to the end
// (separated by a newline), and writes the result as a new version.
// The document must already exist. expectedVersion must be >= 1.
// Returns ErrConflict if expectedVersion does not match the current version.
func (s *Store) Append(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	return s.AppendChecked(&storefmt.WriteSpec{Path: reqPath, ExpectedVersion: expectedVersion, Content: content, Metadata: meta})
}

// AppendChecked is Append with an optional check run on the joined write.
func (s *Store) AppendChecked(spec *storefmt.WriteSpec) (*storefmt.Document, error) {
	reqPath, expectedVersion, content, meta := spec.Path, spec.ExpectedVersion, spec.Content, spec.Metadata
	if expectedVersion < 1 {
		return nil, fmt.Errorf("APPEND requires expected-version >= 1, got %d", expectedVersion)
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("APPEND requires non-empty content")
	}
	if err := storefmt.ValidateMeta(meta); err != nil {
		return nil, err
	}
	if storefmt.ContainsDotDot(reqPath) {
		return nil, os.ErrNotExist
	}

	// Read the document at the expected version so the append is built
	// against the exact base the client saw, avoiding TOCTOU races.
	baseDoc, err := s.Get(reqPath, expectedVersion)
	if err != nil {
		// If the requested version doesn't exist, check whether the
		// document simply moved past it (conflict) or doesn't exist at all.
		current, currentErr := s.CurrentVersionResult(reqPath)
		if currentErr != nil {
			return nil, currentErr
		}
		if current > 0 && current != expectedVersion {
			return &storefmt.Document{Version: current}, storefmt.ErrConflict
		}
		return nil, err
	}

	combined, err := storefmt.JoinContent(baseDoc.Content, content)
	if err != nil {
		return nil, err
	}

	// Write validates the merged map: both sides pass the caps individually,
	// their union need not.
	return s.WriteChecked(&storefmt.WriteSpec{
		Path: reqPath, ExpectedVersion: expectedVersion, Content: combined,
		Metadata: storefmt.PrepareAppendMeta(reqPath, baseDoc.Metadata, meta), Check: spec.Check,
	})
}

// VerifyChain checks the hash chain integrity for a document.
// It reads each version file from oldest to newest and verifies that
// the previous-hash recorded in vN matches the SHA-256 of vN-1's raw bytes.
// Returns nil if the chain is intact, or an error describing the first broken link.
func (s *Store) VerifyChain(reqPath string) error {
	versions, err := s.Versions(reqPath)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	// Sort oldest-first for sequential verification.
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version < versions[j].Version
	})

	loc, err := s.locateContained(reqPath)
	if err != nil {
		return fmt.Errorf("verify chain %s: %w", reqPath, err)
	}

	var previousData []byte
	// Delay chain errors until all files are read so missing files retain precedence.
	var chainErr, formatErr error
	for index, current := range versions {
		currentFile := loc.versionFile(current.Version)
		currentData, err := os.ReadFile(currentFile)
		if err != nil {
			return fmt.Errorf("read v%d: %w", current.Version, err)
		}
		if index > 0 && chainErr == nil {
			recorded := storefmt.ExtractPreviousHash(currentData)
			if recorded == "" {
				chainErr = fmt.Errorf("%w: v%d missing previous-hash", storefmt.ErrIntegrity, current.Version)
			} else if expected := "sha256-" + storefmt.StoredETag(previousData); recorded != expected {
				chainErr = fmt.Errorf("%w: v%d chain broken: previous-hash mismatch (want %s, got %s)",
					storefmt.ErrIntegrity, current.Version, expected, recorded)
			}
		}
		if formatErr == nil {
			header, inspectErr := storefmt.InspectStoredVersion(currentData)
			switch {
			case inspectErr != nil:
				formatErr = fmt.Errorf("%w: v%d: %v", storefmt.ErrIntegrity, current.Version, inspectErr)
			case header.Version != current.Version:
				formatErr = fmt.Errorf("%w: v%d stored version is %d", storefmt.ErrIntegrity, current.Version, header.Version)
			}
		}
		previousData = currentData
	}
	if chainErr != nil {
		return chainErr
	}
	return formatErr
}

// resolveNonExistent resolves a path that doesn't exist yet by walking up
// to find the closest existing ancestor, resolving its symlinks, then
// appending the remaining path segments. This ensures symlink escapes
// through intermediate directories are detected.
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

// buildVersionFile constructs the on-disk bytes for a version file:
// store frontmatter (version, archived, previous-hash, publisher metadata)
// followed by the document content.
func buildVersionFile(versionsDir, base string, version int, content []byte, meta map[string]string) ([]byte, error) {
	var prevData []byte
	if version > 1 {
		prevFile := newVersionFilePath(versionsDir, base, version-1)
		var err error
		prevData, err = os.ReadFile(prevFile)
		if err != nil {
			return nil, fmt.Errorf("read previous version for hashing: %w", err)
		}
	}
	return storefmt.SerializeVersion(version, prevData, content, meta)
}

// migrateLegacyLayout moves legacy versions/{base}.v{N} files to the
// per-doc layout and repairs current pointers. Idempotent; runs from Open
// before any serving. TODO(v1): remove once no pre-per-doc stores remain.
func (s *Store) migrateLegacyLayout() error {
	return filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// Hidden directories are walked too: FETCH serves explicit hidden
		// paths (/.well-known/agent-manifest.md), so their legacy versions
		// must migrate; skipping them broke the manifest on soul.demarkus.io.
		if d.Name() != versionsDirName {
			return nil
		}
		if merr := s.migrateLegacyVersionsDir(path); merr != nil {
			return merr
		}
		return filepath.SkipDir // per-doc subdirs hold only version files
	})
}

// legacyFile pairs a legacy version file's on-disk name with its parsed
// version: the two can diverge ("doc.md.v01" parses to 1), and a
// reconstructed name would miss the file and abort the migration.
type legacyFile struct {
	name string
	n    int
}

// migrateLegacyVersionsDir migrates every legacy-layout document in one
// versions directory. A single ReadDir groups version files by document;
// the per-base worker gets the list so nothing rescans the directory.
func (s *Store) migrateLegacyVersionsDir(versionsDir string) error {
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", versionsDir, err)
	}
	// One filename per (base, n): normalized collisions ("doc.md.v01" vs
	// "doc.md.v1") resolve to the canonical name; the loser stays on disk
	// as an unserved stray, never deleted, never blocking Open.
	byVersion := make(map[string]map[int]string)
	for _, e := range entries {
		// Regular files only: a symlink named like a version would otherwise
		// be moved into the per-doc dir (redundant defense; version reads
		// reject non-regular entries too, see perDocVersionNumber).
		if !e.Type().IsRegular() {
			continue
		}
		base, n, ok := legacyVersion(e.Name())
		if !ok {
			continue
		}
		if byVersion[base] == nil {
			byVersion[base] = make(map[int]string)
		}
		if _, exists := byVersion[base][n]; !exists || e.Name() == fmt.Sprintf("%s.v%d", base, n) {
			byVersion[base][n] = e.Name()
		}
	}
	for base, versions := range byVersion {
		files := make([]legacyFile, 0, len(versions))
		for n, name := range versions {
			files = append(files, legacyFile{name, n})
		}
		currentFile := filepath.Join(filepath.Dir(versionsDir), base)
		if err := s.migrateToPerDocDir(versionsDir, base, currentFile, files); err != nil {
			return fmt.Errorf("migrate %s: %w", currentFile, err)
		}
	}
	return nil
}

// legacyVersion parses a legacy version filename ({base}.v{N}). Visible
// markdown basenames only: the protocol publishes nothing else, and a stray
// "..v1" (base ".") would aim the current-pointer at the directory itself.
func legacyVersion(name string) (base string, n int, ok bool) {
	i := strings.LastIndex(name, ".v")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(name[i+2:])
	if err != nil || n < 1 {
		return "", 0, false
	}
	base = name[:i]
	if !strings.HasSuffix(base, ".md") || isHiddenEntry(base) {
		return "", 0, false
	}
	return base, n, true
}

// migrateToPerDocDir moves base's legacy version files into its per-doc dir
// and points the current symlink at the highest version present after the
// move (a crash may have split layouts). TODO(v1): remove with migration.
func (s *Store) migrateToPerDocDir(versionsDir, base, currentFile string, files []legacyFile) error {
	// Create per-document subdirectory.
	docDir := filepath.Join(versionsDir, base)
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return fmt.Errorf("create per-doc dir for migration: %w", err)
	}

	for _, f := range files {
		oldPath := filepath.Join(versionsDir, f.name)
		newPath := newVersionFilePath(versionsDir, base, f.n)

		// Destination already exists: a concurrent opener moved this version
		// first (collisions were resolved at grouping time, one name per n).
		if _, err := os.Stat(newPath); err == nil {
			continue
		}

		if err := os.Rename(oldPath, newPath); err != nil {
			// A concurrent opener can win the rename between the Stat above
			// and here; the version arriving is success, not failure.
			if errors.Is(err, os.ErrNotExist) {
				if _, statErr := os.Stat(newPath); statErr == nil {
					continue
				}
			}
			return fmt.Errorf("migrate version %d: %w", f.n, err)
		}
	}

	// Point the current symlink at the highest per-doc version now present —
	// scanned after the moves, so versions a crashed migration already moved
	// are counted and the pointer never regresses.
	if highestVersion := highestPerDocVersion(versionsDir, base); highestVersion > 0 {
		relTarget := newVersionSymlinkTarget(base, highestVersion)
		tmpLink := currentFile + ".tmp"
		_ = os.Remove(tmpLink)
		if err := os.Symlink(relTarget, tmpLink); err != nil {
			return fmt.Errorf("symlink during migration: %w", err)
		}
		if err := os.Rename(tmpLink, currentFile); err != nil {
			_ = os.Remove(tmpLink)
			return fmt.Errorf("rename symlink during migration: %w", err)
		}
	}

	return nil
}

// isCurrentArchived checks separate state, falling back to the legacy tip flag.
// Returns (false, nil) if the file doesn't exist (new document).
// Returns an error for non-ErrNotExist read failures.
func (s *Store) isCurrentArchived(versionsDir, base string, version int) (bool, error) {
	path := newVersionFilePath(versionsDir, base, version)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read version file for archive check: %w", err)
	}
	archived, err := effectiveArchived(filepath.Dir(path), data)
	if err != nil {
		return false, fmt.Errorf("read archive state: %w", err)
	}
	return archived, nil
}
