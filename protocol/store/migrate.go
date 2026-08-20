package store

// Migration surface: export and import a document's raw version history so a
// world can move between store backends with byte-identical stored files.

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StoredVersion is one version's raw stored bytes plus its modified time,
// the complete migration unit: current version and archived flag are both
// derivable (newest version; archived flag lives in the tip's frontmatter).
type StoredVersion struct {
	Version  int
	Stored   []byte
	Modified time.Time
}

// Migrator is the backend-neutral migration contract every store implements.
type Migrator interface {
	ExportDocs(fn func(reqPath string, versions []StoredVersion) error) error
	ImportDoc(reqPath string, versions []StoredVersion) error
}

// ValidateImport checks the shared ImportDoc preconditions and returns the
// storage-relative path: a document path, at least one version, versions
// strictly ascending (a pruned history may start past 1).
func ValidateImport(reqPath string, versions []StoredVersion) (string, error) {
	rel, err := RelPath(reqPath)
	if err != nil {
		return "", err
	}
	if rel == "" || strings.HasSuffix(reqPath, "/") {
		return "", fmt.Errorf("import %s: not a document path", reqPath)
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("import %s: no versions", reqPath)
	}
	for i := 1; i < len(versions); i++ {
		if versions[i].Version <= versions[i-1].Version {
			return "", fmt.Errorf("import %s: versions not strictly ascending", reqPath)
		}
	}
	return rel, nil
}

// DiffExports is the one definition of export equality: same documents and
// version numbers, byte-identical stored files, modified equal to the second
// (the precision the protocol exposes via RFC3339 in VERSIONS).
func DiffExports(want, got map[string][]StoredVersion) error {
	if len(got) != len(want) {
		return fmt.Errorf("document count: got %d, want %d", len(got), len(want))
	}
	for path, wv := range want {
		gv, ok := got[path]
		if !ok {
			return fmt.Errorf("%s: missing", path)
		}
		if len(gv) != len(wv) {
			return fmt.Errorf("%s: %d versions, want %d", path, len(gv), len(wv))
		}
		for i := range wv {
			if gv[i].Version != wv[i].Version {
				return fmt.Errorf("%s[%d]: version %d, want %d", path, i, gv[i].Version, wv[i].Version)
			}
			if !bytes.Equal(gv[i].Stored, wv[i].Stored) {
				return fmt.Errorf("%s v%d: stored bytes differ", path, wv[i].Version)
			}
			w := wv[i].Modified.UTC().Truncate(time.Second)
			if !gv[i].Modified.UTC().Truncate(time.Second).Equal(w) {
				return fmt.Errorf("%s v%d: modified differs", path, wv[i].Version)
			}
		}
	}
	return nil
}

// ExportDocs visits every document, archived included, with its history
// oldest-first and stored bytes verbatim. Unreadable documents are hard
// errors; non-symlink entries are not documents (walkCurrentFiles agrees).
func (s *Store) ExportDocs(fn func(reqPath string, versions []StoredVersion) error) error {
	root, err := s.resolvedRoot()
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "versions" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		reqPath := "/" + filepath.ToSlash(rel)
		versions, err := s.exportVersions(reqPath)
		if err != nil {
			return fmt.Errorf("export %s: %w", reqPath, err)
		}
		return fn(reqPath, versions)
	})
}

// exportVersions reads a document's version files oldest-first.
func (s *Store) exportVersions(reqPath string) ([]StoredVersion, error) {
	infos, err := s.Versions(reqPath)
	if err != nil {
		return nil, err
	}
	versions := make([]StoredVersion, 0, len(infos))
	// Versions returns newest-first; walk it backwards for oldest-first.
	for i := len(infos) - 1; i >= 0; i-- {
		vFile, err := s.VersionFilePath(reqPath, infos[i].Version)
		if err != nil {
			return nil, err
		}
		stored, err := os.ReadFile(vFile)
		if err != nil {
			return nil, err
		}
		versions = append(versions, StoredVersion{Version: infos[i].Version, Stored: stored, Modified: infos[i].Modified})
	}
	return versions, nil
}

// ImportDoc writes a document's version files byte-for-byte, restores their
// modified times, and points the current symlink at the newest version. The
// document must not exist; versions must be non-empty, strictly ascending.
func (s *Store) ImportDoc(reqPath string, versions []StoredVersion) error {
	rel, err := ValidateImport(reqPath, versions)
	if err != nil {
		return err
	}
	currentFile := filepath.Join(s.root, rel)
	if _, err := os.Lstat(currentFile); err == nil {
		return fmt.Errorf("import %s: %w", reqPath, os.ErrExist)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("import %s: %w", reqPath, err)
	}

	// The per-doc versions dir, derived from the layout's one owner.
	firstFile, err := s.VersionFilePath(reqPath, versions[0].Version)
	if err != nil {
		return err
	}
	docDir := filepath.Dir(firstFile)
	if err := s.importVersionFiles(reqPath, docDir, versions); err != nil {
		// The document did not exist, so its per-doc dir is all ours to
		// remove: a failed import must not leave partial state behind.
		_ = os.RemoveAll(docDir)
		return err
	}
	newest := versions[len(versions)-1]
	base := filepath.Base(rel)
	if err := os.Symlink(newVersionSymlinkTarget(base, newest.Version), currentFile); err != nil {
		_ = os.RemoveAll(docDir)
		return fmt.Errorf("import %s: %w", reqPath, err)
	}
	if IsArchived(newest.Stored) {
		s.RemoveHashEntry(reqPath)
	} else {
		s.UpdateHashIndex(reqPath, ExtractBody(newest.Stored))
	}
	return nil
}

// importVersionFiles writes the version files byte-for-byte with their
// original modified times.
func (s *Store) importVersionFiles(reqPath, docDir string, versions []StoredVersion) error {
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return fmt.Errorf("import %s: %w", reqPath, err)
	}
	for _, v := range versions {
		vFile, err := s.VersionFilePath(reqPath, v.Version)
		if err != nil {
			return err
		}
		// O_EXCL: version files are immutable; never overwrite one.
		f, err := os.OpenFile(vFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("import %s v%d: %w", reqPath, v.Version, err)
		}
		if _, err := f.Write(v.Stored); err != nil {
			_ = f.Close()
			return fmt.Errorf("import %s v%d: %w", reqPath, v.Version, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("import %s v%d: %w", reqPath, v.Version, err)
		}
		// Version modified times are read from file mtime; restore them.
		if err := os.Chtimes(vFile, v.Modified, v.Modified); err != nil {
			return fmt.Errorf("import %s v%d: %w", reqPath, v.Version, err)
		}
	}
	return nil
}
