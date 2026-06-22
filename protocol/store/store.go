// Package store provides versioned document storage for the Demarkus server.
//
// The store manages documents in a content directory. Only documents written
// through the protocol (with a versions directory) are served. Flat files
// without version history are treated as non-existent.
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
// The store also supports a legacy flat layout (versions/doc.md.v{N}) for
// backward compatibility. Old-layout documents are migrated to the per-document
// subdirectory layout on the next write.
// TODO(v1): remove flat layout support and migration code.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// Document holds a document's content and metadata.
type Document struct {
	Content  []byte
	Modified time.Time
	Version  int
	Archived bool
	Metadata map[string]string
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

// isPerDocLayout reports whether a document uses the per-document subdirectory
// layout (versions/{base}/v{N}) rather than the flat layout (versions/{base}.v{N}).
// TODO(v1): remove once all stores have been migrated; assume per-doc layout.
func isPerDocLayout(versionsDir, base string) bool {
	info, err := os.Stat(filepath.Join(versionsDir, base))
	return err == nil && info.IsDir()
}

// resolveVersionFile returns the absolute path to an existing version file,
// checking per-doc layout first then falling back to flat layout. This handles
// partially-migrated stores where some versions may still be in the old layout.
// TODO(v1): remove flat layout fallback; always use per-doc path directly.
func resolveVersionFile(versionsDir, base string, version int) string {
	perDoc := filepath.Join(versionsDir, base, fmt.Sprintf("v%d", version))
	if _, err := os.Stat(perDoc); err == nil {
		return perDoc
	}
	flat := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, version))
	if _, err := os.Stat(flat); err == nil {
		return flat
	}
	// Neither exists; return per-doc path so callers get a meaningful error.
	return perDoc
}

// newVersionFilePath returns the absolute path for a new version file,
// always using the per-document subdirectory layout.
func newVersionFilePath(versionsDir, base string, version int) string {
	return filepath.Join(versionsDir, base, fmt.Sprintf("v%d", version))
}

// newVersionSymlinkTarget returns the relative symlink target for a new version,
// always using the per-document subdirectory layout.
func newVersionSymlinkTarget(base string, version int) string {
	return filepath.Join("versions", base, fmt.Sprintf("v%d", version))
}

// Store provides read access to a versioned document directory.
type Store struct {
	root    string
	hashMu  sync.RWMutex
	hashIdx map[string]string // content hash → request path
	pathIdx map[string]string // request path → content hash (reverse index)
}

// New creates a store rooted at the given directory.
func New(root string) *Store {
	return &Store{
		root:    root,
		hashIdx: make(map[string]string),
		pathIdx: make(map[string]string),
	}
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

	return filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
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
			return nil // skip broken symlinks
		}
		if !isContained(resolved, absRoot) {
			return nil // skip symlinks that escape the content root
		}
		info, err := os.Stat(resolved)
		if err != nil || info.Size() > int64(protocol.MaxBodyLength+maxStoreFrontmatter) {
			return nil // skip unreadable or oversized files
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil // skip unreadable files
		}
		if isArchived(data) {
			return nil
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return nil
		}
		return fn("/"+rel, data, info.ModTime().UTC().Truncate(time.Second))
	})
}

// BuildHashIndex walks the content root and indexes current versions by content hash.
// Skips versions/ directories and archived documents.
func (s *Store) BuildHashIndex() error {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()

	s.hashIdx = make(map[string]string)
	s.pathIdx = make(map[string]string)

	return s.walkCurrentFiles(func(reqPath string, data []byte, _ time.Time) error {
		hash := contentHash(extractBody(data))
		s.hashIdx[hash] = reqPath
		s.pathIdx[reqPath] = hash
		return nil
	})
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

// LookupHash returns the request path for a content hash, or false if not found.
func (s *Store) LookupHash(hash string) (string, bool) {
	s.hashMu.RLock()
	defer s.hashMu.RUnlock()
	path, ok := s.hashIdx[hash]
	return path, ok
}

// UpdateHashIndex adds or updates the hash index entry for a document.
func (s *Store) UpdateHashIndex(reqPath string, body []byte) {
	hash := contentHash(body)
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	// Remove old hash entry for this path (content changed)
	if oldHash, ok := s.pathIdx[reqPath]; ok {
		delete(s.hashIdx, oldHash)
	}
	s.hashIdx[hash] = reqPath
	s.pathIdx[reqPath] = hash
}

// RemoveHashEntry removes the hash index entry for a given request path.
func (s *Store) RemoveHashEntry(reqPath string) {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	if hash, ok := s.pathIdx[reqPath]; ok {
		delete(s.hashIdx, hash)
		delete(s.pathIdx, reqPath)
	}
}

// HashIndexSize returns the number of entries in the hash index.
func (s *Store) HashIndexSize() int {
	s.hashMu.RLock()
	defer s.hashMu.RUnlock()
	return len(s.hashIdx)
}

// Root returns the content directory path.
func (s *Store) Root() string {
	return s.root
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

	ver := s.CurrentVersion(reqPath)

	return &Document{
		Content:  data,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  ver,
		Archived: isArchived(data),
		Metadata: extractMetadata(data),
	}, nil
}

// ListDir returns directory entries at the given path, excluding dot-files.
func (s *Store) ListDir(reqPath string) ([]os.DirEntry, error) {
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

	// Filter dot-files and the versions directory.
	filtered := entries[:0]
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "versions" {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, nil
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
	if containsDotDot(reqPath) {
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

// CurrentVersion returns the latest version number for a document.
// Returns 0 if no version history exists.
func (s *Store) CurrentVersion(reqPath string) int {
	versions := s.findVersions(reqPath)
	if len(versions) == 0 {
		return 0
	}
	latest := 0
	for _, v := range versions {
		if v.Version > latest {
			latest = v.Version
		}
	}
	return latest
}

// findVersions looks for versioned files in the versions directory.
// Supports both per-document subdirectory layout (versions/{base}/v{N})
// and flat layout (versions/{base}.v{N}).
// Returns nil if no versions directory or no matching files exist.
func (s *Store) findVersions(reqPath string) []VersionInfo {
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)

	versionsDir := filepath.Join(s.root, filepath.Dir(cleaned), "versions")

	if isPerDocLayout(versionsDir, base) {
		return s.findVersionsPerDoc(versionsDir, base)
	}
	return s.findVersionsFlat(versionsDir, base)
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
		if e.IsDir() || !strings.HasPrefix(e.Name(), "v") {
			continue
		}
		numStr := e.Name()[1:] // strip "v" prefix
		num, err := strconv.Atoi(numStr)
		if err != nil || num < 1 {
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

// findVersionsFlat reads versions from the flat layout.
// TODO(v1): remove this function; all stores will use per-doc layout.
func (s *Store) findVersionsFlat(versionsDir, base string) []VersionInfo {
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return nil
	}

	prefix := base + ".v"
	var versions []VersionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		numStr := strings.TrimPrefix(e.Name(), prefix)
		num, err := strconv.Atoi(numStr)
		if err != nil || num < 1 {
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
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)

	// Try per-doc layout first, fall back to flat layout for backward compatibility.
	// TODO(v1): remove flat layout fallback.
	dir := filepath.Dir(cleaned)
	perDocPath := "/" + filepath.Join(dir, "versions", base, fmt.Sprintf("v%d", version))
	filePath, err := s.resolve(perDocPath)
	if err == nil {
		if _, statErr := os.Stat(filePath); statErr != nil {
			filePath = "" // per-doc path resolved but file doesn't exist; try flat
		}
	}
	if filePath == "" {
		flatPath := "/" + filepath.Join(dir, "versions", fmt.Sprintf("%s.v%d", base, version))
		filePath, err = s.resolve(flatPath)
		if err != nil {
			return nil, err
		}
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
		Content:  data,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  version,
		Archived: isArchived(data),
		Metadata: extractMetadata(data),
	}, nil
}

// Archive marks the current version of a document as archived by updating
// the archived flag in its frontmatter. This prevents FETCH from serving
// the document but preserves all version history.
//
// This intentionally modifies the current version file in-place rather than
// creating a new version. The archived flag is operational metadata, not
// content — creating a new version would pollute the history with identical
// content. The hash chain remains valid because only subsequent versions
// hash their predecessor, and the current version (tip) has no successor yet.
func (s *Store) Archive(reqPath string, archived bool) error {
	if _, err := s.resolve(reqPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("resolve path: %w", err)
	}

	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)
	dir := filepath.Dir(cleaned)

	currentVersion := s.CurrentVersion(reqPath)
	if currentVersion == 0 {
		return os.ErrNotExist
	}

	// Resolve the version file path, trying per-doc layout first.
	// TODO(v1): remove flat layout fallback.
	versionsDir := filepath.Join(s.root, dir, "versions")
	resolvedFile := resolveVersionFile(versionsDir, base, currentVersion)
	rel, _ := filepath.Rel(s.root, resolvedFile)
	versionRelPath := "/" + rel
	versionFile, err := s.resolve(versionRelPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("resolve version file: %w", err)
	}

	// Read current version file
	data, err := os.ReadFile(versionFile)
	if err != nil {
		return fmt.Errorf("read version file: %w", err)
	}

	// Update archived flag in frontmatter via manual string parsing.
	// buildVersionFile constructs frontmatter from scratch for new versions but
	// has no inverse (parse-modify-serialize). Introducing a full round-trip
	// parser just for this one toggle would add complexity with no benefit, so
	// we update the single field in-place.
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return fmt.Errorf("invalid version file format")
	}

	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return fmt.Errorf("invalid version file format")
	}

	frontmatter := content[4 : 4+end]
	rest := content[4+end+5:]

	// Parse and update frontmatter
	lines := strings.Split(frontmatter, "\n")
	found := false
	for i, line := range lines {
		key, _, ok := strings.Cut(line, ": ")
		if ok && strings.TrimSpace(key) == "archived" {
			if archived {
				lines[i] = "archived: true"
			} else {
				lines[i] = "archived: false"
			}
			found = true
			break
		}
	}
	if !found {
		// Add archived field if not present
		if archived {
			lines = append(lines, "archived: true")
		} else {
			lines = append(lines, "archived: false")
		}
	}

	// Reconstruct the file
	newFrontmatter := strings.Join(lines, "\n")
	newContent := "---\n" + newFrontmatter + "\n---\n" + rest

	// Atomic write: temp file + rename to avoid partial reads on concurrent FETCH.
	tmp := versionFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("write temp version file: %w", err)
	}
	if err := os.Rename(tmp, versionFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename version file: %w", err)
	}

	if archived {
		s.RemoveHashEntry(reqPath)
	} else {
		body := extractBody(data)
		s.UpdateHashIndex(reqPath, body)
	}

	return nil
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
	if int64(len(content)) > protocol.MaxBodyLength {
		return nil, fmt.Errorf("content exceeds size limit")
	}
	if err := validateMeta(meta); err != nil {
		return nil, err
	}

	// Validate path stays within the store root (resolve handles traversal + symlinks).
	if _, err := s.resolve(reqPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)
	dir := filepath.Dir(cleaned)

	versionsDir := filepath.Join(s.root, dir, "versions")
	if err := os.MkdirAll(versionsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create versions dir: %w", err)
	}

	// Determine the next version number. For a truly new document (no current
	// file on disk), start at 1. Otherwise increment from the current version.
	currentFile := filepath.Join(s.root, dir, base)
	var next int
	if _, err := os.Stat(currentFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			next = 1
		} else {
			return nil, fmt.Errorf("stat current file: %w", err)
		}
	} else {
		next = s.CurrentVersion(reqPath) + 1
	}

	// For existing documents: reject writes to archived documents (closes
	// TOCTOU gap with handler), run migrations, and check for duplicate content.
	if next > 1 {
		doc, err := s.prepareExistingDoc(versionsDir, base, currentFile, next, content, meta)
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
		return nil, fmt.Errorf("content exceeds size limit")
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

	return &Document{
		Content:  content,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  next,
		Archived: false,
		Metadata: meta,
	}, nil
}

// prepareExistingDoc handles pre-write checks for existing documents:
// rejects archived documents, migrates flat files and old layout to per-doc
// subdirectories, and returns ErrNotModified if content is unchanged.
// Returns (nil, nil) when the caller should proceed with writing a new version.
func (s *Store) prepareExistingDoc(versionsDir, base, currentFile string, next int, content []byte, meta map[string]string) (*Document, error) {
	archived, err := s.isCurrentArchived(versionsDir, base, next-1)
	if err != nil {
		return nil, err
	}
	if archived {
		return nil, ErrArchived
	}
	if err := s.migrateFlatFile(versionsDir, base, currentFile); err != nil {
		return nil, err
	}
	if !isPerDocLayout(versionsDir, base) {
		if err := s.migrateToPerDocDir(versionsDir, base, currentFile); err != nil {
			return nil, err
		}
	}

	// Skip creating a new version if content and metadata are identical.
	// After migration, always use per-doc layout for reads.
	prevFile := newVersionFilePath(versionsDir, base, next-1)
	prevData, err := os.ReadFile(prevFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read previous version: %w", err)
		}
		// File doesn't exist — proceed with writing the new version.
		return nil, nil
	}
	if bytes.Equal(extractBody(prevData), content) && metaEqual(extractMetadata(prevData), meta) {
		info, err := os.Stat(prevFile)
		if err != nil {
			return nil, fmt.Errorf("stat current version: %w", err)
		}
		return &Document{
			Content:  content,
			Modified: info.ModTime().UTC().Truncate(time.Second),
			Version:  next - 1,
			Archived: false,
			Metadata: meta,
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
	if expectedVersion < 0 {
		return s.Write(reqPath, content, meta)
	}

	current := s.CurrentVersion(reqPath)
	if current != expectedVersion {
		return &Document{Version: current}, ErrConflict
	}

	doc, err := s.Write(reqPath, content, meta)
	if err != nil {
		if errors.Is(err, ErrVersionExists) {
			// Lost the O_EXCL race: another writer created the expected
			// next version between our check and the file create.
			current = s.CurrentVersion(reqPath)
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
	// correct hash chain, just created under stale assumptions.
	if doc.Version != expectedVersion+1 {
		return &Document{Version: doc.Version}, ErrConflict
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
	if containsDotDot(reqPath) {
		return nil, os.ErrNotExist
	}

	// Read the document at the expected version so the append is built
	// against the exact base the client saw, avoiding TOCTOU races.
	baseDoc, err := s.Get(reqPath, expectedVersion)
	if err != nil {
		// If the requested version doesn't exist, check whether the
		// document simply moved past it (conflict) or doesn't exist at all.
		current := s.CurrentVersion(reqPath)
		if current > 0 && current != expectedVersion {
			return &Document{Version: current}, ErrConflict
		}
		return nil, err
	}

	existing := extractBody(baseDoc.Content)
	combined, err := joinContent(existing, content)
	if err != nil {
		return nil, err
	}

	return s.WriteVersion(reqPath, expectedVersion, combined, meta)
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
	if len(versions) < 2 {
		return nil // nothing to verify
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

	for i, curr := range versions[1:] {
		prev := versions[i]

		prevFile := resolveVersionFile(versionsDir, base, prev.Version)
		currFile := resolveVersionFile(versionsDir, base, curr.Version)

		prevData, err := os.ReadFile(prevFile)
		if err != nil {
			return fmt.Errorf("read v%d: %w", prev.Version, err)
		}
		h := sha256.Sum256(prevData)
		expected := fmt.Sprintf("sha256-%x", h)

		currData, err := os.ReadFile(currFile)
		if err != nil {
			return fmt.Errorf("read v%d: %w", curr.Version, err)
		}
		recorded := extractPreviousHash(currData)
		if recorded == "" {
			return fmt.Errorf("v%d missing previous-hash", curr.Version)
		}
		if recorded != expected {
			return fmt.Errorf("v%d chain broken: previous-hash mismatch (want %s, got %s)",
				curr.Version, expected, recorded)
		}
	}
	return nil
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
	return filepath.Join(append([]string{resolved}, tail...)...), nil
}

// containsDotDot reports whether the path contains a ".." segment.
func containsDotDot(path string) bool {
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
	if err := validateMeta(meta); err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("version: %d\n", version))
	sb.WriteString("archived: false\n")
	if version > 1 {
		prevFile := resolveVersionFile(versionsDir, base, version-1)
		prevData, err := os.ReadFile(prevFile)
		if err != nil {
			return nil, fmt.Errorf("read previous version for hashing: %w", err)
		}
		h := sha256.Sum256(prevData)
		sb.WriteString(fmt.Sprintf("previous-hash: sha256-%x\n", h))
	}
	if len(meta) > 0 {
		keys := make([]string, 0, len(meta))
		for k := range meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch {
			case k == "tags" && okfKeys[k]:
				sb.WriteString(fmt.Sprintf("tags: %s\n", formatTagsList(meta[k])))
			case okfKeys[k]:
				sb.WriteString(fmt.Sprintf("%s: %s\n", k, meta[k]))
			default:
				sb.WriteString(fmt.Sprintf("%s%s: %s\n", metaPrefix, k, meta[k]))
			}
		}
	}
	sb.WriteString("---\n")
	return append([]byte(sb.String()), content...), nil
}

// ValidateMeta checks publisher metadata against the store's key, value, count,
// and size rules. It is exported so a caller that mutates metadata after the
// handler's initial validation (e.g. injecting a default OKF type) can re-check
// before the write and surface a precise error rather than a generic failure.
func ValidateMeta(meta map[string]string) error { return validateMeta(meta) }

// validateMeta checks that metadata keys and values are safe for frontmatter
// serialization. This is defense in depth — the handler also validates, but
// the store is a public API callable outside the network path.
func validateMeta(meta map[string]string) error {
	size := 0
	for k, v := range meta {
		if reservedMetaKeys[k] {
			return fmt.Errorf("metadata key %q is reserved by the store", k)
		}
		if !protocol.IsValidMetaKey(k) {
			return fmt.Errorf("metadata key %q contains invalid characters", k)
		}
		if !protocol.IsValidMetaValue(v) {
			return fmt.Errorf("metadata value for key %q contains newlines", k)
		}
		size += SerializedMetaSize(k, v)
	}
	if len(meta) > protocol.MaxMetaKeys {
		return fmt.Errorf("too many metadata keys (max %d)", protocol.MaxMetaKeys)
	}
	if size > protocol.MaxMetaBytes {
		return fmt.Errorf("metadata too large (max %d bytes)", protocol.MaxMetaBytes)
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

// migrateFlatFile promotes a flat file (no version history) to v1 in the
// versions directory using the per-document subdirectory layout.
// If v1 already exists (concurrent write), it is a no-op.
// TODO(v1): remove this function; flat files without version history won't exist.
func (s *Store) migrateFlatFile(versionsDir, base, currentFile string) error {
	// Check both layouts for an existing v1.
	perDocV1 := newVersionFilePath(versionsDir, base, 1)
	if _, err := os.Stat(perDocV1); !errors.Is(err, os.ErrNotExist) {
		return nil // v1 already exists (new layout)
	}
	flatV1 := filepath.Join(versionsDir, fmt.Sprintf("%s.v1", base))
	if _, err := os.Stat(flatV1); !errors.Is(err, os.ErrNotExist) {
		return nil // v1 already exists (old layout, will be migrated by migrateToPerDocDir)
	}
	flatData, err := os.ReadFile(currentFile)
	if err != nil {
		return fmt.Errorf("read flat file for migration: %w", err)
	}

	// Create per-document subdirectory.
	docDir := filepath.Join(versionsDir, base)
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return fmt.Errorf("create per-doc dir for migration: %w", err)
	}

	v1File := newVersionFilePath(versionsDir, base, 1)
	v1Data := []byte("---\nversion: 1\n---\n")
	v1Data = append(v1Data, flatData...)
	// Use exclusive create to prevent overwriting a v1 that appeared
	// between the Stat check and now (TOCTOU race).
	f, err := os.OpenFile(v1File, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil // v1 was created concurrently
		}
		return fmt.Errorf("migrate flat file to v1: %w", err)
	}
	if _, err := f.Write(v1Data); err != nil {
		_ = f.Close()
		_ = os.Remove(v1File)
		return fmt.Errorf("migrate flat file to v1: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(v1File)
		return fmt.Errorf("migrate flat file to v1: %w", err)
	}
	return nil
}

// migrateToPerDocDir moves version files from the flat layout (versions/{base}.v{N})
// to the per-document subdirectory layout (versions/{base}/v{N}).
// Updates the current symlink to point at the new location.
// No-op if already using per-document layout or no flat files exist.
// TODO(v1): remove this function; all stores will use per-doc layout.
func (s *Store) migrateToPerDocDir(versionsDir, base, currentFile string) error {
	// Create per-document subdirectory.
	docDir := filepath.Join(versionsDir, base)
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return fmt.Errorf("create per-doc dir for migration: %w", err)
	}

	// Find all flat-layout version files for this document.
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return fmt.Errorf("read versions dir for migration: %w", err)
	}

	prefix := base + ".v"
	var highestVersion int
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		numStr := strings.TrimPrefix(e.Name(), prefix)
		num, err := strconv.Atoi(numStr)
		if err != nil || num < 1 {
			continue
		}

		oldPath := filepath.Join(versionsDir, e.Name())
		newPath := newVersionFilePath(versionsDir, base, num)

		// Skip if already migrated (e.g. concurrent migration).
		if _, err := os.Stat(newPath); err == nil {
			_ = os.Remove(oldPath)
			if num > highestVersion {
				highestVersion = num
			}
			continue
		}

		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("migrate version %d: %w", num, err)
		}
		if num > highestVersion {
			highestVersion = num
		}
	}

	// Update symlink to point at the new location.
	if highestVersion > 0 {
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

// isCurrentArchived checks whether the given version file is archived.
// Returns (false, nil) if the file doesn't exist (new document).
// Returns an error for non-ErrNotExist read failures.
func (s *Store) isCurrentArchived(versionsDir, base string, version int) (bool, error) {
	path := resolveVersionFile(versionsDir, base, version)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read version file for archive check: %w", err)
	}
	return isArchived(data), nil
}
