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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Document holds a document's content and metadata.
type Document struct {
	Content  []byte
	Modified time.Time
	Version  int
	Archived bool
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

// MaxFileSize is the maximum file size the store will read (10 MB).
const MaxFileSize = 10 * 1024 * 1024

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
	if info.Size() > MaxFileSize {
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
	if info.Size() > MaxFileSize {
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
func (s *Store) Write(reqPath string, content []byte) (*Document, error) {
	if int64(len(content)) > MaxFileSize {
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

		// Skip creating a new version if content is identical to current.
		prevFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, next-1))
		prevData, err := os.ReadFile(prevFile)
		if err == nil {
			if bytes.Equal(extractBody(prevData), content) {
				info, err := os.Stat(prevFile)
				if err != nil {
					return nil, fmt.Errorf("stat current version: %w", err)
				}
				return &Document{
					Content:  prevData,
					Modified: info.ModTime().UTC().Truncate(time.Second),
					Version:  next - 1,
				}, ErrNotModified
			}
		}
	}

	versionFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, next))

	// Build stored bytes: store frontmatter + original content.
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("version: %d\n", next))
	sb.WriteString("archived: false\n")
	if next > 1 {
		prevFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, next-1))
		prevData, err := os.ReadFile(prevFile)
		if err != nil {
			return nil, fmt.Errorf("read previous version for hashing: %w", err)
		}
		h := sha256.Sum256(prevData)
		sb.WriteString(fmt.Sprintf("previous-hash: sha256-%x\n", h))
	}
	sb.WriteString("---\n")
	stored := append([]byte(sb.String()), content...)

	// Validate stored size after prepending frontmatter.
	if int64(len(stored)) > MaxFileSize {
		return nil, fmt.Errorf("content exceeds size limit")
	}

	// Immutability guard + atomic write: O_CREATE|O_EXCL fails if the file
	// already exists, preventing TOCTOU races between a stat check and rename.
	f, err := os.OpenFile(versionFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("version %d already exists", next)
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
	}, nil
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

// extractBody returns the content after the store frontmatter.
// If no frontmatter is found, the entire data is returned.
func extractBody(data []byte) []byte {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return data
	}
	end := strings.Index(s[4:], "\n---\n")
	if end == -1 {
		return data
	}
	return []byte(s[4+end+5:])
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
