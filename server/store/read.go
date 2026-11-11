package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

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
	if !archived && s.aliased(reqPath) {
		return nil, os.ErrNotExist
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

// ReadCurrent is Get pinned to one target: the pointer is read once, and
// content, version and modified time come from the file it named even
// while a commit swaps it. A current file that is no pointer is no document.
func (s *Store) ReadCurrent(reqPath string) (*storefmt.Document, error) {
	loc, err := s.locate(reqPath)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(loc.current)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return nil, os.ErrNotExist
	}
	target, err := os.Readlink(loc.current)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(loc.current), target)
	}
	versionFile, err := s.contain(target)
	if err != nil {
		return nil, err
	}
	// The target must be a version file in this document's own directory,
	// both resolved the same way. A directory that matches only case-folded
	// is another document's on a case-folding disk: not-found even unindexed.
	docDir, err := s.contain(loc.docDir)
	if err != nil {
		return nil, err
	}
	version := versionNumber(filepath.Base(versionFile))
	if targetDir := filepath.Dir(versionFile); version == 0 || targetDir != docDir {
		if version > 0 && strings.EqualFold(targetDir, docDir) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("%w: current pointer of %s names %s", storefmt.ErrIntegrity, reqPath, target)
	}
	info, err := os.Stat(versionFile)
	if err != nil {
		return nil, err
	}
	if info.Size() > int64(protocol.MaxBodyLength+storefmt.MaxStoreFrontmatter) {
		return nil, fmt.Errorf("%w: file exceeds size limit", storefmt.ErrSizeLimit)
	}
	data, err := os.ReadFile(versionFile)
	if err != nil {
		return nil, err
	}
	archived, err := effectiveArchived(docDir, data)
	if err != nil {
		return nil, err
	}
	if !archived && s.aliased(reqPath) {
		return nil, os.ErrNotExist
	}
	return &storefmt.Document{
		Content:  storefmt.ExtractBody(data),
		Modified: info.ModTime().UTC().Truncate(time.Second),
		Version:  version,
		Archived: archived,
		Metadata: storefmt.ExtractMetadata(data),
		ETag:     storefmt.StoredETag(data),
	}, nil
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
	return docVersion(loc.versionsDir, loc.base)
}

// docVersion returns the highest version in base's per-doc dir (names-only,
// no stats); a missing dir is 0. Any other read failure is an error: a write
// that took it for "no versions" would pass a create-only check.
func docVersion(versionsDir, base string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(versionsDir, base))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read versions of %s: %w", base, err)
	}
	highest := 0
	for _, e := range entries {
		if n := perDocVersionNumber(e); n > highest {
			highest = n
		}
	}
	return highest, nil
}

// highestPerDocVersion is docVersion for a listing: an unreadable dir is 0,
// so an inaccessible doc hides rather than serves broken.
func highestPerDocVersion(versionsDir, base string) int {
	highest, err := docVersion(versionsDir, base)
	if err != nil {
		return 0
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
	return versionNumber(e.Name())
}

// versionNumber is the version a per-document file name "vN" carries, 0
// for any other name.
func versionNumber(name string) int {
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
	if s.liveAlias(reqPath, loc.current) {
		return nil, os.ErrNotExist
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

// VerifyChain reads a document's versions oldest first and checks that each
// previous-hash is the SHA-256 of the version before it. The error names the
// first broken link.
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
	docPath := storefmt.CanonicalPath(reqPath)

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
			if inspectErr == nil {
				inspectErr = header.CheckNames(docPath, current.Version)
			}
			if inspectErr != nil {
				formatErr = fmt.Errorf("%w: v%d: %v", storefmt.ErrIntegrity, current.Version, inspectErr)
			}
		}
		previousData = currentData
	}
	if chainErr != nil {
		return chainErr
	}
	return formatErr
}
