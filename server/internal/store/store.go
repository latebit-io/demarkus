// Package store provides versioned document storage for the Demarkus server.
//
// The store manages documents in a content directory. Only documents written
// through the protocol (with a versions directory) are served. Flat files
// without version history are treated as non-existent.
//
// Layout:
//
//	root/
//	  doc.md              ← symlink to versions/doc.md.v3
//	  versions/
//	    doc.md.v1
//	    doc.md.v2
//	    doc.md.v3
package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/latebit/demarkus/protocol"
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
// metadata, and delimiters).
const maxStoreFrontmatter = 1024

// metaPrefix is the key prefix for publisher-provided metadata in store
// frontmatter. On disk: "meta.type: journal". Stripped when returned to clients.
const metaPrefix = "meta."

// Store provides read access to a versioned document directory.
type Store struct {
	root string
}

// New creates a store rooted at the given directory.
func New(root string) *Store {
	return &Store{root: root}
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

	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root symlinks: %w", err)
	}
	absRoot = resolved

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

	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) {
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
// Returns nil if no versions directory or no matching files exist.
func (s *Store) findVersions(reqPath string) []VersionInfo {
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	base := filepath.Base(cleaned)

	versionsDir := filepath.Join(s.root, filepath.Dir(cleaned), "versions")
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

	// Build path relative to root: versions/doc.md.v1
	versionPath := filepath.Join(filepath.Dir(cleaned), "versions", fmt.Sprintf("%s.v%d", base, version))
	filePath, err := s.resolve("/" + versionPath)
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
		if os.IsNotExist(err) {
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

	versionRelPath := "/" + filepath.Join(dir, "versions", fmt.Sprintf("%s.v%d", base, currentVersion))
	versionFile, err := s.resolve(versionRelPath)
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return fmt.Errorf("resolve version file: %w", err)
	}

	// Read current version file
	data, err := os.ReadFile(versionFile)
	if err != nil {
		return fmt.Errorf("read version file: %w", err)
	}

	// Update archived flag in frontmatter
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
//	---
//	<original content>
//
// The previous-hash is the SHA-256 of the raw on-disk bytes of version N-1,
// forming a hash chain that allows chain integrity to be verified later.
func (s *Store) Write(reqPath string, content []byte, meta map[string]string) (*Document, error) {
	if int64(len(content)) > protocol.MaxBodyLength {
		return nil, fmt.Errorf("content exceeds size limit")
	}

	// Validate path stays within the store root (resolve handles traversal + symlinks).
	if _, err := s.resolve(reqPath); err != nil {
		if os.IsNotExist(err) {
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
		if os.IsNotExist(err) {
			next = 1
		} else {
			return nil, fmt.Errorf("stat current file: %w", err)
		}
	} else {
		next = s.CurrentVersion(reqPath) + 1
	}

	// For existing documents: reject writes to archived documents (closes
	// TOCTOU gap with handler) and migrate flat files to v1 if needed.
	if next > 1 {
		if s.isCurrentArchived(versionsDir, base, next-1) {
			return nil, ErrArchived
		}
		if err := s.migrateFlatFile(versionsDir, base, currentFile); err != nil {
			return nil, err
		}

		// Skip creating a new version if content and metadata are identical.
		prevFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, next-1))
		prevData, err := os.ReadFile(prevFile)
		if err == nil {
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
		}
	}

	versionFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, next))

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
	f, err := os.OpenFile(versionFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("version %d: %w", next, ErrVersionExists)
		}
		return nil, fmt.Errorf("create version file: %w", err)
	}
	if _, err := f.Write(stored); err != nil {
		_ = f.Close()
		_ = os.Remove(versionFile)
		return nil, fmt.Errorf("write version file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(versionFile)
		return nil, fmt.Errorf("close version file: %w", err)
	}

	// Atomically update the current file to point at the new version.
	// Create a temp symlink then rename over the current path so readers
	// never see a missing file. Use a relative target so the content
	// directory can be relocated without breaking links.
	relTarget := filepath.Join("versions", fmt.Sprintf("%s.v%d", base, next))
	tmpLink := currentFile + ".tmp"
	_ = os.Remove(tmpLink) // clean up any stale temp link
	if err := os.Symlink(relTarget, tmpLink); err != nil {
		return nil, fmt.Errorf("symlink current file: %w", err)
	}
	if err := os.Rename(tmpLink, currentFile); err != nil {
		_ = os.Remove(tmpLink)
		return nil, fmt.Errorf("rename current file: %w", err)
	}

	info, err := os.Stat(versionFile)
	if err != nil {
		return nil, fmt.Errorf("stat version file: %w", err)
	}

	return &Document{
		Content:  content,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  next,
		Archived: false,
		Metadata: meta,
	}, nil
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

		prevFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, prev.Version))
		currFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, curr.Version))

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
		prevFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, version-1))
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
			sb.WriteString(fmt.Sprintf("%s%s: %s\n", metaPrefix, k, meta[k]))
		}
	}
	sb.WriteString("---\n")
	return append([]byte(sb.String()), content...), nil
}

// validateMeta checks that metadata keys and values are safe for frontmatter
// serialization. This is defense in depth — the handler also validates, but
// the store is a public API callable outside the network path.
func validateMeta(meta map[string]string) error {
	for k, v := range meta {
		if !protocol.IsValidMetaKey(k) {
			return fmt.Errorf("metadata key %q contains invalid characters", k)
		}
		if !protocol.IsValidMetaValue(v) {
			return fmt.Errorf("metadata value for key %q contains newlines", k)
		}
	}
	return nil
}

// extractMetadata parses publisher metadata from store frontmatter.
// Returns keys with the "meta." prefix stripped, or nil if none found.
func extractMetadata(data []byte) map[string]string {
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return nil
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return nil
	}
	block := content[4 : 4+end]
	var meta map[string]string
	for line := range strings.SplitSeq(block, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if strings.HasPrefix(key, metaPrefix) {
			if meta == nil {
				meta = make(map[string]string)
			}
			meta[key[len(metaPrefix):]] = strings.TrimSpace(val)
		}
	}
	return meta
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
// versions directory. If v1 already exists (concurrent write), it is a no-op.
func (s *Store) migrateFlatFile(versionsDir, base, currentFile string) error {
	v1File := filepath.Join(versionsDir, fmt.Sprintf("%s.v1", base))
	if _, err := os.Stat(v1File); !os.IsNotExist(err) {
		return nil // v1 already exists
	}
	flatData, err := os.ReadFile(currentFile)
	if err != nil {
		return fmt.Errorf("read flat file for migration: %w", err)
	}
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

// isCurrentArchived checks whether the given version file is archived.
func (s *Store) isCurrentArchived(versionsDir, base string, version int) bool {
	path := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, version))
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return isArchived(data)
}
