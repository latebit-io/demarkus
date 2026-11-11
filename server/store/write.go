package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// newVersionFilePath returns the absolute path for a new version file,
// always using the per-document subdirectory layout.
func newVersionFilePath(versionsDir, base string, version int) string {
	return filepath.Join(versionsDir, base, fmt.Sprintf("v%d", version))
}

// newVersionSymlinkTarget returns the relative symlink target for a new version,
// always using the per-document subdirectory layout.
func newVersionSymlinkTarget(base string, version int) string {
	return filepath.Join(versionsDirName, base, fmt.Sprintf("v%d", version))
}

// Write creates a new immutable version file; existing versions are never
// modified. The stored format (store fields, the previous-hash chain, the
// path) is SPEC §9.4.
func (s *Store) Write(reqPath string, content []byte, meta map[string]string) (*storefmt.Document, error) {
	if err := storefmt.ValidateWrite(content, meta); err != nil {
		return nil, err
	}
	return s.write(reqPath, content, meta, nil)
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
	if err := s.checkVersionsDir(versionsDir); err != nil {
		return nil, err
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
		if s.liveAlias(reqPath, currentFile) {
			return nil, ErrPathAlias
		}
		// containWrite already held the versions tree inside the root.
		highest, err := docVersion(versionsDir, base)
		if err != nil {
			return nil, err
		}
		next = highest + 1
	}

	// For existing documents: reject writes to archived documents (closes
	// TOCTOU gap with handler) and check for duplicate content.
	if next > 1 {
		doc, err := s.prepareExistingDoc(&loc, next, content, meta)
		if doc != nil || err != nil {
			return doc, err
		}
	}

	vFile := loc.versionFile(next)
	docPath := storefmt.CanonicalPath(reqPath)

	stored, err := buildVersionFile(&loc, &storefmt.VersionSpec{Path: docPath, Version: next, Content: content, Metadata: meta})
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
		prepared := storefmt.PreparedWrite{Path: docPath, Content: content, Metadata: persisted}
		if err := check(prepared); err != nil {
			return nil, err
		}
	}

	// Create per-document subdirectory for new documents.
	docDir := loc.docDir
	if err := s.mkdirAllDurable(docDir); err != nil {
		return nil, fmt.Errorf("create per-doc versions dir: %w", err)
	}

	modTime, err := createVersionFile(vFile, stored, next)
	if err != nil {
		return nil, err
	}
	var unsynced error
	if applied, err := swapCurrent(currentFile, newVersionSymlinkTarget(base, next)); err != nil {
		if !applied {
			// An unreferenced version would fail every later write with O_EXCL.
			return nil, errors.Join(err, removeIfPresent(vFile))
		}
		// The pointer moved: the version is current and readers see it.
		unsynced = fmt.Errorf("%w: %w", storefmt.ErrCommittedNotSynced, err)
	}

	s.UpdateIndex(reqPath, content, next)

	// Metadata is the persisted form (what Get returns), not the request
	// map: "alpha, beta" is stored as a tags list and reads back "alpha,beta",
	// and the catalog must see one spelling whichever path populated it.
	doc := &storefmt.Document{
		Content:  content,
		Modified: modTime.UTC().Truncate(time.Second),
		Version:  next,
		Archived: false,
		Metadata: persisted,
		ETag:     storefmt.StoredETag(stored),
	}
	if keep := storefmt.RetentionValue(meta); keep > 0 {
		doc.Prune = s.pruneVersions(versionsDir, base, next, keep)
	}
	return doc, unsynced
}

// pruneVersions keeps the newest keep versions, deleting oldest first and
// stopping at the first failure so survivors stay a contiguous suffix. Removals
// go through an os.Root at the store root; a planted symlink cannot redirect one.
func (s *Store) pruneVersions(versionsDir, base string, current, keep int) (res *storefmt.PruneResult) {
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
	// No logger at this layer: the close error rides in the result the caller audit-logs.
	defer func() {
		if err := rootFS.Close(); err != nil {
			if res == nil {
				res = &storefmt.PruneResult{}
			}
			res.Err = errors.Join(res.Err, fmt.Errorf("prune: close store root: %w", err))
		}
	}()

	versions := s.findVersionsPerDoc(versionsDir, base)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version < versions[j].Version
	})
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
func (s *Store) prepareExistingDoc(loc *docLocation, next int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	prevFile := loc.versionFile(next - 1)
	prevData, err := os.ReadFile(prevFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read previous version: %w", err)
		}
		return nil, nil // no previous version: write the new one
	}
	archived, err := effectiveArchived(filepath.Dir(prevFile), prevData)
	if err != nil {
		return nil, fmt.Errorf("read archive state: %w", err)
	}
	if archived {
		return nil, storefmt.ErrArchived
	}

	// Skip creating a new version if content and metadata are identical.
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

// WriteVersion is Write under an expected version: negative skips the check,
// 0 creates only, N updates only version N. A mismatch is ErrConflict.
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
	if !storefmt.Committed(err) {
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
			// The content matches the current version, which a concurrent
			// writer may have moved past expectedVersion+1: not-modified
			// holds only at the version expected.
			if doc.Version != expectedVersion && doc.Version != expectedVersion+1 {
				return &storefmt.Document{Version: doc.Version}, storefmt.ErrConflict
			}
		}
		return doc, err
	}

	// A writer slipping in between the pre-check and the version computation
	// lands this version past expectedVersion+1. It is current with a valid
	// chain, so the whole document goes back under a conflict.
	if doc.Version != expectedVersion+1 {
		return doc, errors.Join(storefmt.ErrConflict, storefmt.ErrCommittedStale, err)
	}

	return doc, err
}

// Append joins content to the document at expectedVersion (>= 1, newline
// separated) as a new version. A version that moved on is ErrConflict.
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

// buildVersionFile serializes spec, filling its chain link from the version
// file before it.
func buildVersionFile(loc *docLocation, spec *storefmt.VersionSpec) ([]byte, error) {
	if spec.Version > 1 {
		previous, err := os.ReadFile(loc.versionFile(spec.Version - 1))
		if err != nil {
			return nil, fmt.Errorf("read previous version for hashing: %w", err)
		}
		spec.Previous = previous
	}
	return storefmt.SerializeVersion(spec)
}
