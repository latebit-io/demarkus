// Package store provides versioned document storage for the Demarkus server.
//
// The store manages documents in a content directory, supporting both flat-file
// layouts (single version per document) and versioned layouts (multiple immutable
// versions per document).
//
// Flat layout (current, read-only):
//
//	root/
//	  doc.md              ← plain file, treated as version 1
//
// Versioned layout (future, write-enabled):
//
//	root/
//	  doc.md              ← symlink to versions/doc.md.v3
//	  versions/
//	    doc.md.v1
//	    doc.md.v2
//	    doc.md.v3
package store

import (
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
}

// VersionInfo describes a single version of a document.
type VersionInfo struct {
	Version  int
	Modified time.Time
}

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
// current version. Returns os.ErrNotExist if the document is not found.
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

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	ver := s.CurrentVersion(reqPath)

	return &Document{
		Content:  data,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  ver,
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
// For flat files (no versions directory), returns a single entry.
func (s *Store) Versions(reqPath string) ([]VersionInfo, error) {
	// Check the document exists at all.
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

	// Look for versioned files in the versions directory.
	versions := s.findVersions(reqPath)
	if len(versions) > 0 {
		// Sort newest first.
		sort.Slice(versions, func(i, j int) bool {
			return versions[i].Version > versions[j].Version
		})
		return versions, nil
	}

	// Flat file: report as version 1.
	return []VersionInfo{{
		Version:  1,
		Modified: info.ModTime().UTC().Truncate(time.Second),
	}}, nil
}

// resolve validates and resolves a request path to an absolute filesystem path
// within the content directory. Returns os.ErrNotExist for invalid paths.
func (s *Store) resolve(reqPath string) (string, error) {
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
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
		// Path doesn't exist yet — resolve symlinks on the parent directory
		// (which should exist) to get a canonical base, then append the filename.
		// This prevents the /var → /private/var mismatch on macOS.
		parent := filepath.Dir(joined)
		resolvedParent, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			resolvedParent, perr = filepath.Abs(parent)
			if perr != nil {
				return "", perr
			}
		}
		absPath = filepath.Join(resolvedParent, filepath.Base(joined))
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
// For flat files, returns 1. For versioned documents, returns the highest version.
func (s *Store) CurrentVersion(reqPath string) int {
	versions := s.findVersions(reqPath)
	if len(versions) == 0 {
		return 1
	}
	max := 0
	for _, v := range versions {
		if v.Version > max {
			max = v.Version
		}
	}
	return max
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
	}, nil
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
		return nil, os.ErrNotExist
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
	if _, err := os.Stat(currentFile); os.IsNotExist(err) {
		next = 1
	} else {
		next = s.CurrentVersion(reqPath) + 1
	}

	// Flat file migration: if this is a versioned write (next > 1) but v1 doesn't
	// exist in the versions dir yet, migrate the flat file content to v1 first.
	if next > 1 {
		v1File := filepath.Join(versionsDir, fmt.Sprintf("%s.v1", base))
		if _, err := os.Stat(v1File); os.IsNotExist(err) {
			flatData, err := os.ReadFile(currentFile)
			if err != nil {
				return nil, fmt.Errorf("read flat file for migration: %w", err)
			}
			var v1sb strings.Builder
			v1sb.WriteString("---\nversion: 1\n---\n")
			v1Data := append([]byte(v1sb.String()), flatData...)
			if err := os.WriteFile(v1File, v1Data, 0o644); err != nil {
				return nil, fmt.Errorf("migrate flat file to v1: %w", err)
			}
		}
	}

	versionFile := filepath.Join(versionsDir, fmt.Sprintf("%s.v%d", base, next))

	// Immutability guard: never overwrite an existing version.
	if _, err := os.Stat(versionFile); err == nil {
		return nil, fmt.Errorf("version %d already exists", next)
	}

	// Build stored bytes: store frontmatter + original content.
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("version: %d\n", next))
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

	// Write atomically: temp file in the same directory, then rename.
	tmp, err := os.CreateTemp(versionsDir, ".tmp-")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(stored); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("write version file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("close version file: %w", err)
	}
	if err := os.Rename(tmpName, versionFile); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("rename version file: %w", err)
	}

	// Update the current file: symlink to the version file.
	// Remove any existing file/symlink first (os.Symlink fails if target exists).
	os.Remove(currentFile)
	if err := os.Symlink(versionFile, currentFile); err != nil {
		return nil, fmt.Errorf("symlink current file: %w", err)
	}

	info, err := os.Stat(versionFile)
	if err != nil {
		return nil, fmt.Errorf("stat version file: %w", err)
	}

	return &Document{
		Content:  content,
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  next,
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

	for i := 1; i < len(versions); i++ {
		prev := versions[i-1]
		curr := versions[i]

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
	for _, line := range strings.Split(block, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if ok && strings.TrimSpace(key) == "previous-hash" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}
