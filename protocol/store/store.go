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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	slashpath "path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol"
)

// Document holds a document's content and metadata. Content is always the
// body only — store frontmatter is stripped on every path (Get, Write,
// WriteVersion, Append); operational state travels in the dedicated fields.
type Document struct {
	Content  []byte
	Modified time.Time
	Version  int
	Archived bool
	Metadata map[string]string
	ETag     string
	// Prune reports retention pruning performed by the write that produced
	// this document, or nil when no pruning ran. Callers on the network path
	// audit-log it so version deletions are always attributable.
	Prune *PruneResult
}

// PruneResult describes the contiguous range of oldest versions deleted by
// retention pruning after a successful write. From/To are zero when a
// deletion failure stopped pruning before anything was removed. Err is
// non-nil when pruning stopped early; versions From..To were already deleted
// and the rest remain a contiguous suffix, so the hash chain stays
// verifiable.
type PruneResult struct {
	From int
	To   int
	Err  error
}

// VersionInfo describes a single version of a document.
type VersionInfo struct {
	Version  int
	Modified time.Time
}

// ErrArchived is returned by Write when the document is archived.
var ErrArchived = fmt.Errorf("document is archived")

// ErrNotModified is returned by Write when the content is identical
// to the current version, making the publish a no-op.
var ErrNotModified = fmt.Errorf("content not modified")

// ErrConflict is returned by WriteVersion when the expected version
// does not match the current version (optimistic concurrency check).
var ErrConflict = fmt.Errorf("version conflict")

// ErrVersionExists is returned by Write when the computed next version
// file already exists (O_EXCL race with a concurrent writer).
var ErrVersionExists = fmt.Errorf("version already exists")

// ErrSizeLimit is returned when combined content exceeds protocol.MaxBodyLength.
var ErrSizeLimit = fmt.Errorf("combined content exceeds size limit")

// ErrInvalidPath is returned when a publish path does not end in .md. The
// protocol serves markdown documents; a non-.md path is not a document (SPEC 2.3).
var ErrInvalidPath = fmt.Errorf("publish path must end in .md")

// ErrInvalidContent is returned when a document body is not valid UTF-8.
// Markdown is UTF-8 text; binary content is not a markdown document (SPEC 2.3, 4.4).
var ErrInvalidContent = fmt.Errorf("document body must be valid UTF-8 text")

// ErrInvalidMeta is returned when publisher metadata breaks the key, value,
// count, or size rules. Writes surface it past a caller's pre-check, since
// APPEND's merge (SPEC 6.6) can breach caps both sides satisfied alone.
var ErrInvalidMeta = fmt.Errorf("invalid publisher metadata")

// ErrIntegrity marks verified stored state whose bytes or hash chain are corrupt.
var ErrIntegrity = fmt.Errorf("stored version integrity failure")

// maxStoreFrontmatter is the maximum overhead the store-managed frontmatter
// adds to a version file (version, archived, previous-hash, publisher
// metadata, and delimiters). It must cover MaxMetaBytes of publisher metadata
// plus the operational fields and per-line serialization overhead.
const maxStoreFrontmatter = 2048

// metaPrefix is the key prefix for non-spec publisher metadata in store
// frontmatter. On disk: "meta.importance: 0.8". Stripped when returned to
// clients. Keys recognized by the Open Knowledge Format (see okfKeys) are
// written bare instead, so the store frontmatter matches the OKF spec for the
// fields it defines.
const metaPrefix = "meta."

const archiveStateName = "archive-state"

// okfKeys are the publisher metadata keys recognized by the Open Knowledge
// Format (OKF) spec. They serialize as bare frontmatter fields (e.g. "tags:")
// to conform to the spec; every other publisher key keeps the metaPrefix. The
// in-memory metadata map is bare-keyed either way — only on-disk serialization
// differs.
var okfKeys = map[string]bool{
	"type":        true,
	"title":       true,
	"description": true,
	"resource":    true,
	"tags":        true,
	"timestamp":   true,
}

// retentionKey is the publisher metadata key that bounds a document's version
// history. When the just-written version carries it, the write prunes the
// oldest versions so at most that many remain (current included). It is
// publisher metadata, not a reserved store field: any writer with publish
// capability may set it, the same trust level that can archive the document.
// Absent retention means keep every version — the default is unchanged.
const retentionKey = "retention"

// reservedMetaKeys are bare frontmatter fields owned by the store. Publishers
// may not set them (validateMeta rejects them) and extractMetadata never
// surfaces them as publisher metadata, preventing a publisher from forging
// store state such as version or archival.
var reservedMetaKeys = map[string]bool{
	"version":       true,
	"previous-hash": true,
	"archived":      true,
}

// IsReservedMetaKey reports whether a publisher metadata key is reserved by the
// store and would be rejected on publish. Callers that build metadata from
// untrusted sources (e.g. importing external documents) use this to drop or
// rename colliding keys before publishing.
func IsReservedMetaKey(key string) bool {
	return reservedMetaKeys[key]
}

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
		return false, false, fmt.Errorf("%w: invalid archive state file", ErrIntegrity)
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
		return false, false, fmt.Errorf("%w: invalid archive state %q", ErrIntegrity, data)
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
	return isArchived(tipStored), nil
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

// versionRelPath is the root-relative location of one version of rel:
// <dir>/versions/<base>/vN.
func versionRelPath(rel string, version int) string {
	return filepath.Join(filepath.Dir(rel), "versions", filepath.Base(rel), fmt.Sprintf("v%d", version))
}

// newVersionSymlinkTarget returns the relative symlink target for a new version,
// always using the per-document subdirectory layout.
func newVersionSymlinkTarget(base string, version int) string {
	return filepath.Join("versions", base, fmt.Sprintf("v%d", version))
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

// contentHash computes the sha256 content hash for a document body.
func contentHash(body []byte) string {
	h := sha256.Sum256(body)
	return "sha256-" + hex.EncodeToString(h[:])
}

// walkCurrentFiles walks the content root and calls fn for each current,
// non-archived document version. It follows current-version symlinks, skips
// the versions/ directory, and skips archived, oversized, or unreadable files.
// data is the raw on-disk bytes (including store frontmatter). This is the
// shared traversal used by both BuildHashIndex and WalkCurrent so the skip and
// containment rules live in one place.
func (s *Store) walkCurrentFiles(fn func(reqPath string, data []byte, modified time.Time) error) error {
	absRoot, err := s.resolvedRoot()
	if err != nil {
		return err
	}

	// Entries that cannot be indexed are skipped, never fatal: one broken
	// symlink must not take the server down. They are reported as a
	// PartialWalkError so the caller can surface the degradation.
	partial := &PartialWalkError{}
	skip := func(path string, err error) error {
		partial.Total++
		if len(partial.Skipped) < maxSkippedSample {
			partial.Skipped = append(partial.Skipped, SkippedEntry{Path: path, Err: err})
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
			if d.Name() == "versions" {
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
		if info.Size() > int64(protocol.MaxBodyLength+maxStoreFrontmatter) {
			return skip(path, ErrSizeLimit)
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

// maxSkippedSample bounds the entries a PartialWalkError retains; a badly
// damaged root must not exhaust memory during startup.
const maxSkippedSample = 32

var errSymlinkEscapes = errors.New("current-version symlink escapes the content root")

// SkippedEntry is a content-root entry a walk could not index.
type SkippedEntry struct {
	Path string
	Err  error
}

// PartialWalkError reports a walk that completed but skipped entries: the
// derived index is missing them. Callers log it rather than treating the
// build as whole or aborting. Skipped holds at most maxSkippedSample entries.
type PartialWalkError struct {
	Total   int
	Skipped []SkippedEntry
}

func (e *PartialWalkError) Error() string {
	return fmt.Sprintf("walk skipped %d entries (first %s: %v)", e.Total, e.Skipped[0].Path, e.Skipped[0].Err)
}

// BuildHashIndex walks the content root and indexes current versions by content hash.
// Skips versions/ directories and archived documents.
func (s *Store) BuildHashIndex() error {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()

	s.hashIdx = make(map[string][]string)
	s.pathIdx = make(map[string]string)
	s.hashErr = nil

	err := s.walkCurrentFiles(func(reqPath string, data []byte, _ time.Time) error {
		s.indexLocked(reqPath, contentHash(extractBody(data)))
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

// CurrentDoc describes a current, non-archived document version surfaced by
// WalkCurrent. Body is the markdown content with store frontmatter stripped;
// Metadata is the publisher metadata as a bare-keyed map (see extractMetadata).
type CurrentDoc struct {
	Path     string
	Body     []byte
	Metadata map[string]string
	Modified time.Time
}

// WalkCurrent visits every current, non-archived document version and calls fn
// for each. It mirrors BuildHashIndex's traversal: it follows current-version
// symlinks, skips the versions/ directory, and skips archived, oversized, or
// unreadable files. This lets a caller (e.g. the LOOKUP catalog) build its own
// derived index from the same source of truth without re-implementing the walk.
func (s *Store) WalkCurrent(fn func(CurrentDoc) error) error {
	return s.walkCurrentFiles(func(reqPath string, data []byte, modified time.Time) error {
		return fn(CurrentDoc{
			Path:     reqPath,
			Body:     extractBody(data),
			Metadata: extractMetadata(data),
			Modified: modified,
		})
	})
}

// LookupHash returns a live request path and whether one exists.
//
// Deprecated: use LookupHashResult to distinguish misses from index errors.
func (s *Store) LookupHash(hash string) (string, bool) {
	path, err := s.LookupHashResult(hash)
	return path, err == nil
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
	s.indexLocked(CanonicalPath(reqPath), contentHash(body))
}

// RemoveHashEntry removes the hash index entry for a given request path.
func (s *Store) RemoveHashEntry(reqPath string) {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	s.unindexLocked(CanonicalPath(reqPath))
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
	rel, err := RelPath(reqPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, versionRelPath(rel, version)), nil
}

// Get retrieves a document at the given path. If version is 0, returns the
// current version. Only serves documents with a versions directory — flat files
// without version history are treated as non-existent.
func (s *Store) Get(reqPath string, version int) (*Document, error) {
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
	if info.Size() > int64(protocol.MaxBodyLength+maxStoreFrontmatter) {
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

	return &Document{
		Content:  extractBody(data),
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  ver,
		Archived: archived,
		Metadata: extractMetadata(data),
		ETag:     StoredETag(data),
	}, nil
}

// DirEntry is one entry in a directory listing. It is backend-neutral: a
// store implementation that is not a filesystem can still produce it.
type DirEntry struct {
	Name  string
	IsDir bool
}

// ListEntries returns the backend-neutral directory listing used by the
// server's DocumentStore contract. It applies the same filtering as ListDir.
func (s *Store) ListEntries(reqPath string, includeArchived bool) ([]DirEntry, error) {
	entries, err := s.ListDir(reqPath, includeArchived)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

// ListDir returns entries at reqPath, excluding dot-files, versions/, and
// non-documents (SPEC 9.8/11.9). includeArchived=false also omits archived
// docs and document-free subtrees; true is the recovery/audit view.
func (s *Store) ListDir(reqPath string, includeArchived bool) ([]os.DirEntry, error) {
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
	for _, e := range entries {
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
		entries, err := os.ReadDir(filepath.Join(dc.dirAbs, "versions"))
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
	return dc.bases[name] && highestPerDocVersion(filepath.Join(dc.dirAbs, "versions"), name) > 0
}

// isHiddenEntry reports whether a directory entry name is always excluded from
// a listing, regardless of archival: dot-files and the per-document versions/
// directory. Shared by ListDir so the exclusion list lives in one place.
func isHiddenEntry(name string) bool {
	return strings.HasPrefix(name, ".") || name == "versions"
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
	canon := CanonicalPath(dirReq)
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
	buf := make([]byte, maxStoreFrontmatter+256)
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
func (s *Store) Versions(reqPath string) ([]VersionInfo, error) {
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
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")

	// Reject paths that contain .. segments. filepath.Clean collapses traversal
	// attempts into valid-looking paths (e.g., /../etc/passwd → etc/passwd), so
	// we check the original path for traversal intent as defense-in-depth.
	if ContainsDotDot(reqPath) {
		return "", os.ErrNotExist
	}

	joined := filepath.Join(s.root, cleaned)

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

// CurrentVersion returns the latest version number, or 0 when none exists.
//
// Deprecated: use CurrentVersionResult to surface invalid paths.
func (s *Store) CurrentVersion(reqPath string) int {
	version, _ := s.CurrentVersionResult(reqPath)
	return version
}

// CurrentVersionResult returns the latest version number and path errors.
func (s *Store) CurrentVersionResult(reqPath string) (int, error) {
	cleaned, err := RelPath(reqPath)
	if err != nil {
		return 0, err
	}
	versionsDir := filepath.Join(s.root, filepath.Dir(cleaned), "versions")
	return highestPerDocVersion(versionsDir, filepath.Base(cleaned)), nil
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
func (s *Store) findVersions(reqPath string) []VersionInfo {
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)

	versionsDir := filepath.Join(s.root, filepath.Dir(cleaned), "versions")
	return s.findVersionsPerDoc(versionsDir, base)
}

// findVersionsPerDoc reads versions from the per-document subdirectory layout.
func (s *Store) findVersionsPerDoc(versionsDir, base string) []VersionInfo {
	docDir := filepath.Join(versionsDir, base)
	entries, err := os.ReadDir(docDir)
	if err != nil {
		return nil
	}

	var versions []VersionInfo
	for _, e := range entries {
		num := perDocVersionNumber(e)
		if num == 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		versions = append(versions, VersionInfo{
			Version:  num,
			Modified: info.ModTime().UTC().Truncate(time.Second),
		})
	}
	return versions
}

// getVersion retrieves a specific version of a document from the versions directory.
// Uses resolve() for path validation — same security as all other path access.
func (s *Store) getVersion(reqPath string, version int) (*Document, error) {
	cleaned, err := RelPath(reqPath)
	if err != nil {
		return nil, err
	}
	filePath, err := s.resolve("/" + versionRelPath(cleaned, version))
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	if info.Size() > int64(protocol.MaxBodyLength+maxStoreFrontmatter) {
		return nil, fmt.Errorf("file exceeds size limit")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	return &Document{
		Content:  extractBody(data),
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  version,
		Archived: false,
		Metadata: extractMetadata(data),
		ETag:     StoredETag(data),
	}, nil
}

// Archive changes operational archive state without modifying version bytes.
//
// Deprecated: use ArchiveResult to inspect the resulting document and change.
func (s *Store) Archive(reqPath string, archived bool) error {
	_, _, err := s.ArchiveResult(reqPath, archived)
	return err
}

// ArchiveResult changes archive state and returns its committed result.
func (s *Store) ArchiveResult(reqPath string, archived bool) (*Document, bool, error) {
	if _, err := s.resolve(reqPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, os.ErrNotExist
		}
		return nil, false, fmt.Errorf("resolve path: %w", err)
	}

	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)
	dir := filepath.Dir(cleaned)

	currentVersion, err := s.CurrentVersionResult(reqPath)
	if err != nil {
		return nil, false, err
	}
	if currentVersion == 0 {
		return nil, false, os.ErrNotExist
	}

	versionsDir := filepath.Join(s.root, dir, "versions")
	resolvedFile := newVersionFilePath(versionsDir, base, currentVersion)
	rel, err := filepath.Rel(s.root, resolvedFile)
	if err != nil {
		return nil, false, fmt.Errorf("resolve version path: %w", err)
	}
	versionFile, err := s.resolve("/" + filepath.ToSlash(rel))
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
	s.UpdateHashIndex(reqPath, extractBody(stored))
}

func documentFromStored(data []byte, modified time.Time, version int, archived bool) *Document {
	return &Document{
		Content:  extractBody(data),
		Modified: modified.UTC().Truncate(time.Second),
		Version:  version,
		Archived: archived,
		Metadata: extractMetadata(data),
		ETag:     StoredETag(data),
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
func (s *Store) Write(reqPath string, content []byte, meta map[string]string) (*Document, error) {
	if err := validateWrite(content, meta); err != nil {
		return nil, err
	}
	return s.write(reqPath, content, meta)
}

// write is the validated core shared by Write and WriteVersion.
func (s *Store) write(reqPath string, content []byte, meta map[string]string) (*Document, error) {
	cleaned, err := RelPath(reqPath)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(cleaned)
	dir := filepath.Dir(cleaned)

	// Validate path stays within the store root (resolve handles traversal + symlinks).
	if _, err := s.resolve(reqPath); err != nil {
		if errors.Is(err, errUnderDocument) {
			// Topology collision, same class as writing over a directory.
			return nil, fmt.Errorf("cannot publish %s: a document exists at an ancestor", reqPath)
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	versionsDir := filepath.Join(s.root, dir, "versions")
	if err := os.MkdirAll(versionsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create versions dir: %w", err)
	}

	// Determine the next version number. For a truly new document (no current
	// file on disk), start at 1. Otherwise increment from the current version.
	currentFile := filepath.Join(s.root, dir, base)
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
		return nil, fmt.Errorf("cannot publish %s: a directory exists at this path", reqPath)
	} else {
		current, err := s.CurrentVersionResult(reqPath)
		if err != nil {
			return nil, err
		}
		next = current + 1
	}

	// For existing documents: reject writes to archived documents (closes
	// TOCTOU gap with handler) and check for duplicate content.
	if next > 1 {
		doc, err := s.prepareExistingDoc(versionsDir, base, next, content, meta)
		if doc != nil || err != nil {
			return doc, err
		}
	}

	// Create per-document subdirectory for new documents.
	docDir := filepath.Join(versionsDir, base)
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return nil, fmt.Errorf("create per-doc versions dir: %w", err)
	}

	vFile := newVersionFilePath(versionsDir, base, next)

	stored, err := buildVersionFile(versionsDir, base, next, content, meta)
	if err != nil {
		return nil, err
	}

	// Validate stored size after prepending frontmatter.
	if int64(len(stored)) > int64(protocol.MaxBodyLength+maxStoreFrontmatter) {
		return nil, ErrSizeLimit
	}

	// Immutability guard + atomic write: O_CREATE|O_EXCL fails if the file
	// already exists, preventing TOCTOU races between a stat check and rename.
	f, err := os.OpenFile(vFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("version %d: %w", next, ErrVersionExists)
		}
		return nil, fmt.Errorf("create version file: %w", err)
	}
	if _, err := f.Write(stored); err != nil {
		_ = f.Close()
		_ = os.Remove(vFile)
		return nil, fmt.Errorf("write version file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(vFile)
		return nil, fmt.Errorf("close version file: %w", err)
	}

	// Atomically update the current file to point at the new version.
	// Create a temp symlink then rename over the current path so readers
	// never see a missing file. Use a relative target so the content
	// directory can be relocated without breaking links.
	relTarget := newVersionSymlinkTarget(base, next)
	tmpLink := currentFile + ".tmp"
	_ = os.Remove(tmpLink) // clean up any stale temp link
	if err := os.Symlink(relTarget, tmpLink); err != nil {
		return nil, fmt.Errorf("symlink current file: %w", err)
	}
	if err := os.Rename(tmpLink, currentFile); err != nil {
		_ = os.Remove(tmpLink)
		return nil, fmt.Errorf("rename current file: %w", err)
	}

	info, err := os.Stat(vFile)
	if err != nil {
		return nil, fmt.Errorf("stat version file: %w", err)
	}

	s.UpdateHashIndex(reqPath, content)

	// Metadata is the persisted form (what Get returns), not the request
	// map: "alpha, beta" is stored as a tags list and reads back "alpha,beta",
	// and the catalog must see one spelling whichever path populated it.
	doc := &Document{
		Content:  content,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  next,
		Archived: false,
		Metadata: extractMetadata(stored),
		ETag:     StoredETag(stored),
	}
	if keep := retentionValue(meta); keep > 0 {
		doc.Prune = s.pruneVersions(versionsDir, base, next, keep)
	}
	return doc, nil
}

// ParseRetention parses a retention metadata value. ok is true only for a
// positive integer — the only form that prunes; validateMeta rejects every
// other value. Exported so clients gating destructive-confirmation UX (the
// CLI prompt) share the exact predicate the server enforces instead of
// re-implementing it and drifting.
func ParseRetention(v string) (n int, ok bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// retentionValue returns the retention count declared in publisher metadata,
// or 0 when absent. validateMeta has already rejected non-integer or < 1
// values, so a parse failure here means the map bypassed validation — treat
// it as no retention rather than pruning on a value that was never vetted.
func retentionValue(meta map[string]string) int {
	v, ok := meta[retentionKey]
	if !ok {
		return 0
	}
	n, ok := ParseRetention(v)
	if !ok {
		return 0
	}
	return n
}

// pruneVersions deletes the oldest versions of a document so at most keep
// versions remain, the just-written current version included. Deletion runs
// oldest-first and stops at the first failure so the surviving versions are
// always a contiguous suffix — VerifyChain never sees a gap. It runs only
// after a successful write, which has already migrated the document to the
// per-doc layout, so only versions/{base}/v{N} files are ever touched.
//
// Deletion is the store's only destructive operation, so it takes no chances
// with the filesystem: version numbers come from ReadDir (never from input),
// filenames are reconstructed as v{N}, and every removal goes through an
// os.Root anchored at the store root. os.Root resolves the whole path inside
// the root at delete time, so a planted symlink (e.g. versions/{base}
// pointing outside the store) or a directory swapped in mid-prune cannot
// redirect a delete to a file outside the store, and a version file that is
// itself a symlink is unlinked, never followed. Returns nil when there was
// nothing to delete.
//
// Deletion is deliberately unbounded per write: the first retained write on a
// long history prunes the whole backlog inline (that is the upgrade path for
// documents that accumulated hundreds of versions before retention existed).
// Steady state deletes one version per write.
func (s *Store) pruneVersions(versionsDir, base string, current, keep int) *PruneResult {
	cutoff := current - keep // delete versions <= cutoff
	if cutoff < 1 {
		return nil
	}
	relDocDir, err := filepath.Rel(s.root, filepath.Join(versionsDir, base))
	if err != nil || strings.HasPrefix(relDocDir, "..") {
		return &PruneResult{Err: fmt.Errorf("prune: doc dir escapes store root: %q", relDocDir)}
	}
	rootFS, err := os.OpenRoot(s.root)
	if err != nil {
		return &PruneResult{Err: fmt.Errorf("prune: open store root: %w", err)}
	}
	// Read-only directory handle; nothing actionable on close failure.
	defer func() { _ = rootFS.Close() }()

	versions := s.findVersionsPerDoc(versionsDir, base)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version < versions[j].Version
	})
	var res *PruneResult
	for _, v := range versions {
		if v.Version > cutoff {
			break
		}
		if err := rootFS.Remove(filepath.Join(relDocDir, fmt.Sprintf("v%d", v.Version))); err != nil {
			if res == nil {
				res = &PruneResult{}
			}
			res.Err = fmt.Errorf("prune v%d: %w", v.Version, err)
			break
		}
		if res == nil {
			res = &PruneResult{From: v.Version}
		}
		res.To = v.Version
	}
	return res
}

// prepareExistingDoc rejects archived documents and returns ErrNotModified
// on unchanged content; (nil, nil) means proceed with a new version.
func (s *Store) prepareExistingDoc(versionsDir, base string, next int, content []byte, meta map[string]string) (*Document, error) {
	archived, err := s.isCurrentArchived(versionsDir, base, next-1)
	if err != nil {
		return nil, err
	}
	if archived {
		return nil, ErrArchived
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
	storedMeta := extractMetadata(prevData)
	if bytes.Equal(extractBody(prevData), content) && metaEqual(storedMeta, NormalizeMetadata(meta)) {
		info, err := os.Stat(prevFile)
		if err != nil {
			return nil, fmt.Errorf("stat current version: %w", err)
		}
		return &Document{
			Content:  content,
			Modified: info.ModTime().UTC().Truncate(time.Second),
			Version:  next - 1,
			Archived: false,
			Metadata: storedMeta,
			ETag:     StoredETag(prevData),
		}, ErrNotModified
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
func (s *Store) WriteVersion(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*Document, error) {
	// Request-shaped checks come before any state read so an invalid write
	// never masquerades as a conflict; every backend must order checks this way.
	if err := validateWrite(content, meta); err != nil {
		return nil, err
	}
	if ContainsDotDot(reqPath) {
		return nil, os.ErrNotExist
	}
	if expectedVersion < 0 {
		return s.write(reqPath, content, meta)
	}

	current, err := s.CurrentVersionResult(reqPath)
	if err != nil {
		return nil, err
	}
	if current != expectedVersion {
		return &Document{Version: current}, ErrConflict
	}

	doc, err := s.write(reqPath, content, meta)
	if err != nil {
		if errors.Is(err, ErrVersionExists) {
			// Lost the O_EXCL race: another writer created the expected
			// next version between our check and the file create.
			current, currentErr := s.CurrentVersionResult(reqPath)
			if currentErr != nil {
				return nil, currentErr
			}
			return &Document{Version: current}, ErrConflict
		}
		if errors.Is(err, ErrNotModified) && doc != nil {
			// Content matches current version — but if a concurrent writer
			// created intervening versions, the "current" version may have
			// moved past expectedVersion+1. Allow not-modified only when
			// the version is what we'd expect.
			if doc.Version != expectedVersion && doc.Version != expectedVersion+1 {
				return &Document{Version: doc.Version}, ErrConflict
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
		return &Document{Version: doc.Version, Prune: doc.Prune}, ErrConflict
	}

	return doc, nil
}

// Append reads the document at expectedVersion, appends content to the end
// (separated by a newline), and writes the result as a new version.
// The document must already exist. expectedVersion must be >= 1.
// Returns ErrConflict if expectedVersion does not match the current version.
func (s *Store) Append(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*Document, error) {
	if expectedVersion < 1 {
		return nil, fmt.Errorf("APPEND requires expected-version >= 1, got %d", expectedVersion)
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("APPEND requires non-empty content")
	}
	if err := validateMeta(meta); err != nil {
		return nil, err
	}
	if ContainsDotDot(reqPath) {
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
			return &Document{Version: current}, ErrConflict
		}
		return nil, err
	}

	combined, err := joinContent(baseDoc.Content, content)
	if err != nil {
		return nil, err
	}

	// Write validates the merged map: both sides pass the caps individually,
	// their union need not.
	return s.WriteVersion(reqPath, expectedVersion, combined, PrepareAppendMeta(reqPath, baseDoc.Metadata, meta))
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

	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)
	dir := filepath.Dir(cleaned)
	versionsDir := filepath.Join(s.root, dir, "versions")

	var previousData []byte
	// Delay chain errors until all files are read so missing files retain precedence.
	var chainErr, formatErr error
	for index, current := range versions {
		currentFile := newVersionFilePath(versionsDir, base, current.Version)
		currentData, err := os.ReadFile(currentFile)
		if err != nil {
			return fmt.Errorf("read v%d: %w", current.Version, err)
		}
		if index > 0 && chainErr == nil {
			recorded := extractPreviousHash(currentData)
			if recorded == "" {
				chainErr = fmt.Errorf("%w: v%d missing previous-hash", ErrIntegrity, current.Version)
			} else if expected := "sha256-" + StoredETag(previousData); recorded != expected {
				chainErr = fmt.Errorf("%w: v%d chain broken: previous-hash mismatch (want %s, got %s)",
					ErrIntegrity, current.Version, expected, recorded)
			}
		}
		if formatErr == nil {
			header, inspectErr := InspectStoredVersion(currentData)
			switch {
			case inspectErr != nil:
				formatErr = fmt.Errorf("%w: v%d: %v", ErrIntegrity, current.Version, inspectErr)
			case header.Version != current.Version:
				formatErr = fmt.Errorf("%w: v%d stored version is %d", ErrIntegrity, current.Version, header.Version)
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

// CanonicalPath is the one spelling of a request path shared by every index
// keyed on paths (hash index, catalog): slash-cleaned, single leading
// slash, root is "/". Callers reject traversal via ContainsDotDot.
func CanonicalPath(reqPath string) string {
	return slashpath.Clean("/" + reqPath)
}

// RelPath is the storage-relative form of a request path: CanonicalPath
// without the leading slash, "" for the root. ".." is rejected with
// os.ErrNotExist so traversal never reaches a backend.
func RelPath(reqPath string) (string, error) {
	if ContainsDotDot(reqPath) {
		return "", os.ErrNotExist
	}
	return strings.TrimPrefix(CanonicalPath(reqPath), "/"), nil
}

// ContainsDotDot reports whether the path contains a ".." segment. Every
// backend rejects such paths with os.ErrNotExist; exported so they share one
// definition of traversal.
func ContainsDotDot(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// extractPreviousHash parses the store frontmatter from raw version file bytes
// and returns the value of the previous-hash field, or "" if absent.
func extractPreviousHash(data []byte) string {
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return ""
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return ""
	}
	block := content[4 : 4+end]
	for line := range strings.SplitSeq(block, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if ok && strings.TrimSpace(key) == "previous-hash" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// isArchived checks if a version file is marked as archived in its frontmatter.
func isArchived(data []byte) bool {
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return false
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return false
	}
	block := content[4 : 4+end]
	for line := range strings.SplitSeq(block, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if ok && strings.TrimSpace(key) == "archived" {
			return strings.TrimSpace(val) == "true"
		}
	}
	return false
}

// joinContent concatenates existing and new content with a newline separator.
// A separator is only added when existing content is non-empty and does not
// already end with a newline. Returns ErrSizeLimit if the result exceeds
// protocol.MaxBodyLength.
func joinContent(existing, content []byte) ([]byte, error) {
	if len(content) == 0 {
		return existing, nil
	}
	sep := 0
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		sep = 1
	}
	n := int64(len(existing)) + int64(sep) + int64(len(content))
	if n > protocol.MaxBodyLength {
		return nil, ErrSizeLimit
	}
	combined := make([]byte, 0, int(n))
	combined = append(combined, existing...)
	if sep == 1 {
		combined = append(combined, '\n')
	}
	combined = append(combined, content...)
	return combined, nil
}

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
	return SerializeVersion(version, prevData, content, meta)
}

// ValidateMeta checks publisher metadata against the store's key, value, count,
// and size rules. It is exported so a caller that mutates metadata after the
// handler's initial validation (e.g. injecting a default OKF type) can re-check
// before the write and surface a precise error rather than a generic failure.
func ValidateMeta(meta map[string]string) error { return validateMeta(meta) }

// mergeAppendMeta layers an APPEND's metadata over the base version's, request
// keys winning (SPEC 6.6); without it an append drops the doc out of LOOKUP.
// retention is never inherited: pruning belongs to the write declaring it.
func mergeAppendMeta(base, req map[string]string) map[string]string {
	if len(base) == 0 && len(req) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(req))
	maps.Copy(merged, base)
	delete(merged, retentionKey)
	maps.Copy(merged, req)
	return merged
}

// ApplyOKFTypeDefault types every written concept document (SPEC 6.4), leaving
// a declared type and the reserved OKF files alone. Copies rather than stamping
// the caller's map, which may belong to a fetched Document.
func ApplyOKFTypeDefault(reqPath string, meta map[string]string) map[string]string {
	switch slashpath.Base(reqPath) {
	case "index.md", "log.md":
		return meta
	}
	if strings.TrimSpace(meta["type"]) != "" {
		return meta
	}
	typed := make(map[string]string, len(meta)+1)
	maps.Copy(typed, meta)
	typed["type"] = protocol.OKFDefaultType
	return typed
}

// ValidateDocumentContent enforces the document contract for PUBLISH/APPEND: a
// .md path with a UTF-8 body. Callers map the errors to bad-request.
func ValidateDocumentContent(reqPath string, content []byte) error {
	if !strings.HasSuffix(reqPath, ".md") {
		return ErrInvalidPath
	}
	return ValidateBody(content)
}

// ValidateBody checks a body is UTF-8 text. Each backend's Write calls it as
// defense in depth. No .md check here: the store is deliberately path-agnostic.
func ValidateBody(content []byte) error {
	if !utf8.Valid(content) {
		return ErrInvalidContent
	}
	return nil
}

// validateWrite checks the size, metadata, and encoding of a write request.
func validateWrite(content []byte, meta map[string]string) error {
	if int64(len(content)) > protocol.MaxBodyLength {
		return ErrSizeLimit
	}
	if err := validateMeta(meta); err != nil {
		return err
	}
	return ValidateBody(content)
}

// validateMeta checks metadata is safe for frontmatter serialization. Defense
// in depth: the handler validates too, but the store is callable off the
// network path. Failures wrap ErrInvalidMeta so callers can classify them.
func validateMeta(meta map[string]string) error {
	size := 0
	for k, v := range meta {
		if reservedMetaKeys[k] {
			return fmt.Errorf("%w: key %q is reserved by the store", ErrInvalidMeta, k)
		}
		if !protocol.IsValidMetaKey(k) {
			return fmt.Errorf("%w: key %q contains invalid characters", ErrInvalidMeta, k)
		}
		if !protocol.IsValidMetaValue(v) {
			return fmt.Errorf("%w: value for key %q is not valid single-line UTF-8", ErrInvalidMeta, k)
		}
		if k == retentionKey {
			if _, ok := ParseRetention(v); !ok {
				return fmt.Errorf("%w: key %q must be a positive integer, got %q", ErrInvalidMeta, retentionKey, v)
			}
		}
		size += SerializedMetaSize(k, v)
	}
	if len(meta) > protocol.MaxMetaKeys {
		return fmt.Errorf("%w: too many metadata keys (max %d)", ErrInvalidMeta, protocol.MaxMetaKeys)
	}
	if size > protocol.MaxMetaBytes {
		return fmt.Errorf("%w: metadata too large (max %d bytes)", ErrInvalidMeta, protocol.MaxMetaBytes)
	}
	return nil
}

// extractMetadata parses publisher metadata from store frontmatter, returning a
// bare-keyed map (or nil if none found). Two on-disk forms are recognized:
// recognized OKF fields written bare (okfKeys, "tags" parsed from its YAML
// list), and every other publisher key carried under metaPrefix (stripped
// here). Reserved store fields (reservedMetaKeys) and any other bare key are
// ignored, so operational state never surfaces as publisher metadata. An older
// "meta."-prefixed tags value is read as-is, keeping prior writes readable.
func extractMetadata(data []byte) map[string]string {
	if len(data) < 4 || !bytes.HasPrefix(data, []byte("---\n")) {
		return nil
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end == -1 {
		return nil
	}
	block := string(data[4 : 4+end])
	var meta map[string]string
	set := func(k, v string) {
		if meta == nil {
			meta = make(map[string]string)
		}
		meta[k] = v
	}
	for line := range strings.SplitSeq(block, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimRight(val, "\r")
		switch {
		case reservedMetaKeys[key]:
			// Operational field — never publisher metadata.
		case strings.HasPrefix(key, metaPrefix):
			// Re-check the unprefixed key so a stored "meta.archived" cannot
			// resurrect a reserved operational field as publisher metadata.
			if k := key[len(metaPrefix):]; !reservedMetaKeys[k] {
				set(k, val)
			}
		case key == "tags":
			set(key, parseTagsList(val))
		case okfKeys[key]:
			set(key, val)
		}
	}
	return meta
}

// SerializedMetaSize returns the byte size a publisher key/value pair counts
// against MaxMetaBytes: the key length plus the value length as actually
// serialized on disk. The OKF "tags" field is stored as a YAML flow list, which
// is longer than its comma-separated map form, so counting the raw value would
// undercount the on-disk size and let a tag-heavy document slip past the budget
// only to overflow the frontmatter. Per-line delimiters and the "meta." prefix
// are fixed, bounded overhead covered by maxStoreFrontmatter, not counted here.
func SerializedMetaSize(key, value string) int {
	if key == "tags" {
		return len(key) + len(formatTagsList(value))
	}
	return len(key) + len(value)
}

// FormatTagsList serializes a comma-separated tag string as an OKF YAML flow
// list. It is exported for the OKF export codec so a bundle's on-disk tags
// representation matches the store's exactly (a single source of truth for the
// serialized form that SerializedMetaSize accounts for).
func FormatTagsList(csv string) string { return formatTagsList(csv) }

// formatTagsList serializes a comma-separated tag string as an OKF YAML flow
// list, e.g. "sales,revenue" → "[sales, revenue]". Empty input yields "[]".
func formatTagsList(csv string) string {
	var tags []string
	for raw := range strings.SplitSeq(csv, ",") {
		if t := strings.TrimSpace(raw); t != "" {
			tags = append(tags, t)
		}
	}
	return "[" + strings.Join(tags, ", ") + "]"
}

// parseTagsList parses a tags value back to the comma-separated form held in the
// metadata map. It accepts both the OKF YAML flow list ("[sales, revenue]") and
// a bare comma-separated string (older "meta.tags" writes), so prior versions
// stay readable.
func parseTagsList(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = v[1 : len(v)-1]
	}
	var tags []string
	for raw := range strings.SplitSeq(v, ",") {
		if t := strings.TrimSpace(raw); t != "" {
			tags = append(tags, t)
		}
	}
	return strings.Join(tags, ",")
}

// metaEqual reports whether two metadata maps are equal.
// Treats nil and empty maps as equal.
func metaEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// extractBody returns the content after the store frontmatter.
// If no frontmatter is found, the entire data is returned.
func extractBody(data []byte) []byte {
	delim := []byte("---\n")
	if !bytes.HasPrefix(data, delim) {
		return data
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end == -1 {
		return data
	}
	return data[4+end+5:]
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
		if d.Name() != "versions" {
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
