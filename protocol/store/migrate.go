package store

// Migration surface: export and import a document's raw version history so a
// world can move between store backends with byte-identical stored files.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// StoredVersion is one version's raw stored bytes plus its modified time.
type StoredVersion struct {
	Version  int
	Stored   []byte
	Modified time.Time
}

// StoredDocument is one document's immutable history and operational archive
// state. Archived is independent from the stored tip bytes.
type StoredDocument struct {
	Versions []StoredVersion
	Archived bool
}

// Migrator is the backend-neutral migration contract; the caller owns the
// cancellation policy via ctx. The ExportDocs callback may retain versions,
// so implementations must hand each document a fresh backing array.
type Migrator interface {
	ExportDocs(ctx context.Context, fn func(reqPath string, document StoredDocument) error) error
	ImportDoc(ctx context.Context, reqPath string, document StoredDocument) error
}

// ValidateImport checks the shared ImportDoc preconditions and returns the
// storage-relative path: a document path, at least one version, versions
// strictly ascending (a pruned history may start past 1), none oversized.
func ValidateImport(reqPath string, document StoredDocument) (string, error) {
	rel, err := RelPath(reqPath)
	if err != nil {
		return "", err
	}
	if rel == "" || strings.HasSuffix(reqPath, "/") {
		return "", fmt.Errorf("import %s: not a document path", reqPath)
	}
	if len(document.Versions) == 0 {
		return "", fmt.Errorf("import %s: no versions", reqPath)
	}
	for i, v := range document.Versions {
		if i > 0 && v.Version <= document.Versions[i-1].Version {
			return "", fmt.Errorf("import %s: versions not strictly ascending", reqPath)
		}
		if int64(len(v.Stored)) > int64(protocol.MaxBodyLength+maxStoreFrontmatter) {
			return "", fmt.Errorf("import %s v%d: %w", reqPath, v.Version, ErrSizeLimit)
		}
	}
	return rel, nil
}

// DiffExports is the one definition of export equality: same documents,
// archive state, version numbers, stored bytes, and second-precision modified
// times (the precision the protocol exposes via RFC3339 in VERSIONS).
func DiffExports(want, got map[string]StoredDocument) error {
	if len(got) != len(want) {
		return fmt.Errorf("document count: got %d, want %d", len(got), len(want))
	}
	for path, wantDocument := range want {
		gotDocument, ok := got[path]
		if !ok {
			return fmt.Errorf("%s: missing", path)
		}
		if gotDocument.Archived != wantDocument.Archived {
			return fmt.Errorf("%s: archived %v, want %v", path, gotDocument.Archived, wantDocument.Archived)
		}
		wv := wantDocument.Versions
		gv := gotDocument.Versions
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
// errors; only symlink entries outside versions dirs count as documents.
func (s *Store) ExportDocs(ctx context.Context, fn func(reqPath string, document StoredDocument) error) error {
	root, err := s.resolvedRoot()
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
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
		newest := versions[len(versions)-1]
		archived, err := s.exportArchived(reqPath, newest.Stored)
		if err != nil {
			return fmt.Errorf("export %s: %w", reqPath, err)
		}
		return fn(reqPath, StoredDocument{Versions: versions, Archived: archived})
	})
}

func (s *Store) exportArchived(reqPath string, tipStored []byte) (bool, error) {
	rel, err := RelPath(reqPath)
	if err != nil {
		return false, err
	}
	docDir := filepath.Join(s.root, filepath.Dir(rel), "versions", filepath.Base(rel))
	return effectiveArchived(docDir, tipStored)
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
func (s *Store) ImportDoc(ctx context.Context, reqPath string, document StoredDocument) error {
	rel, err := ValidateImport(reqPath, document)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	currentFile := filepath.Join(s.root, rel)
	if _, err := os.Lstat(currentFile); err == nil {
		return fmt.Errorf("import %s: %w", reqPath, os.ErrExist)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("import %s: %w", reqPath, err)
	}

	// Mkdir (not MkdirAll) claims the per-doc versions dir atomically: a
	// pre-existing dir (orphaned tree, concurrent import) refuses, so the
	// failure cleanup below can never widen into files this call did not write.
	firstFile, err := s.VersionFilePath(reqPath, document.Versions[0].Version)
	if err != nil {
		return err
	}
	docDir := filepath.Dir(firstFile)
	if err := os.MkdirAll(filepath.Dir(docDir), 0o755); err != nil {
		return fmt.Errorf("import %s: %w", reqPath, err)
	}
	if err := os.Mkdir(docDir, 0o755); err != nil {
		return fmt.Errorf("import %s: version directory: %w", reqPath, err)
	}
	cleanup := func(importErr error) error {
		if cleanupErr := os.RemoveAll(docDir); cleanupErr != nil {
			return errors.Join(importErr, fmt.Errorf("cleanup import %s: %w", reqPath, cleanupErr))
		}
		return importErr
	}
	if err := s.importVersionFiles(ctx, reqPath, document.Versions); err != nil {
		return cleanup(err)
	}
	newest := document.Versions[len(document.Versions)-1]
	base := filepath.Base(rel)
	if err := ctx.Err(); err != nil {
		return cleanup(err)
	}
	if _, err := writeArchiveState(docDir, document.Archived); err != nil {
		return cleanup(fmt.Errorf("import %s: archive state: %w", reqPath, err))
	}
	if err := ctx.Err(); err != nil {
		return cleanup(err)
	}
	if err := os.Symlink(newVersionSymlinkTarget(base, newest.Version), currentFile); err != nil {
		return cleanup(fmt.Errorf("import %s: %w", reqPath, err))
	}
	if document.Archived {
		s.RemoveHashEntry(reqPath)
	} else {
		s.UpdateHashIndex(reqPath, ExtractBody(newest.Stored))
	}
	return nil
}

// importVersionFiles writes the version files byte-for-byte with their
// original modified times, honoring cancellation between files.
func (s *Store) importVersionFiles(ctx context.Context, reqPath string, versions []StoredVersion) error {
	for _, v := range versions {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			// Close error is moot; the write error is what matters.
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
