package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// indexLocked records reqPath (already canonical), replacing any earlier
// entry. Caller holds indexMu.
func (s *Store) indexLocked(reqPath, hash string, version int) {
	s.unindexLocked(reqPath)
	digest := fold(reqPath, version, hash)
	s.pathIdx[reqPath] = digest
	s.xorDigest(digest)
}

// unindexLocked removes reqPath from the index. Caller holds indexMu.
func (s *Store) unindexLocked(reqPath string) {
	digest, ok := s.pathIdx[reqPath]
	if !ok {
		return
	}
	delete(s.pathIdx, reqPath)
	s.xorDigest(digest)
}

// UpdateIndex indexes the current version of a document by body and
// version number. Keys are canonical so "/d/e/" and "/d/e" index one
// document, as BuildIndex derives from the walk.
func (s *Store) UpdateIndex(reqPath string, body []byte, version int) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	s.indexLocked(storefmt.CanonicalPath(reqPath), storefmt.ContentHash(body), version)
}

// RemoveIndexEntry drops a request path from the index.
func (s *Store) RemoveIndexEntry(reqPath string) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	s.unindexLocked(storefmt.CanonicalPath(reqPath))
}

// IndexSize returns the number of indexed documents.
func (s *Store) IndexSize() int {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return len(s.pathIdx)
}

// Indexed reports whether reqPath is a current indexed document.
func (s *Store) Indexed(reqPath string) bool {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	_, ok := s.pathIdx[storefmt.CanonicalPath(reqPath)]
	return ok
}

// ErrPathAlias is a write whose path reaches an existing document under
// another spelling.
var ErrPathAlias = errors.New("path reaches an existing document under another spelling")

// aliased reports a path the disk answers under another spelling (a
// case-folding filesystem), which the permission check never saw. A path the
// index lacks is compared with the names on disk. False until the index is built.
func (s *Store) aliased(reqPath string) bool {
	s.indexMu.RLock()
	_, ok := s.pathIdx[storefmt.CanonicalPath(reqPath)]
	indexed := s.indexed
	s.indexMu.RUnlock()
	if !indexed || ok {
		return false
	}
	return !s.spelledOnDisk(reqPath)
}

// spelledOnDisk reports whether every segment of reqPath is a directory
// entry under exactly that name. A directory that cannot be read is no match.
func (s *Store) spelledOnDisk(reqPath string) bool {
	rel, err := storefmt.RelPath(reqPath)
	if err != nil {
		return false
	}
	if rel == "" {
		return true
	}
	dir := s.root
	for segment := range strings.SplitSeq(rel, "/") {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		// ReadDir sorts by name.
		i := sort.Search(len(entries), func(i int) bool { return entries[i].Name() >= segment })
		if i == len(entries) || entries[i].Name() != segment {
			return false
		}
		dir = filepath.Join(dir, segment)
	}
	return true
}

// liveAlias is aliased for a path whose current file is a live document:
// archived documents leave the index without being aliases, and a flat
// file is no document. A failed look reads as no alias.
func (s *Store) liveAlias(reqPath, current string) bool {
	if !s.aliased(reqPath) {
		return false
	}
	fi, err := os.Lstat(current)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	absRoot, err := s.resolvedRoot()
	if err != nil {
		return false
	}
	archived, err := entryArchived(current, absRoot)
	return err == nil && !archived
}

// generationName is the marker a writer outside the serving process moves, so
// that process rebuilds what it derived from the tree. A dotfile: never listed.
const generationName = ".generation"

// MarkChanged tells a process serving this root that another one wrote it;
// see Generation. A direct writer calls it after its writes.
func (s *Store) MarkChanged() error {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("mark changed: %w", err)
	}
	_, err := WriteFileAtomic(filepath.Join(s.root, generationName), 0o644, func(w io.Writer) error {
		_, err := io.WriteString(w, hex.EncodeToString(raw[:]))
		return err
	})
	if err != nil {
		return fmt.Errorf("mark changed: %w", err)
	}
	return nil
}

// Generation is the marker's value, "" when no outside writer left one. A
// server compares it per read and rebuilds its index when it moved.
func (s *Store) Generation() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.root, generationName))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read generation marker: %w", err)
	}
	return string(data), nil
}

// digestLen is the fingerprint width: change detection, not a security bound.
const digestLen = 16

// fold is an entry's digest contribution. The version is in it so a new
// version with the same body still moves the fingerprint.
func fold(reqPath string, version int, hash string) (out [digestLen]byte) {
	var buf [256]byte // typical paths fit, so the input needs no heap
	input := append(buf[:0], reqPath...)
	input = append(input, 0)
	input = strconv.AppendInt(input, int64(version), 10)
	input = append(input, 0)
	input = append(input, hash...)
	sum := sha256.Sum256(input)
	copy(out[:], sum[:digestLen])
	return out
}

// xorDigest toggles one fold in the digest; XOR is its own inverse.
func (s *Store) xorDigest(f [digestLen]byte) { xorInto(&s.digest, f) }

func xorInto(dst *[digestLen]byte, f [digestLen]byte) {
	for i, b := range f {
		dst[i] ^= b
	}
}

// indexVersion is a stored file's version for the index; a header the
// parser refuses folds as 0, and VerifyChain reports that file's corruption.
func indexVersion(stored []byte) int {
	version, err := storefmt.StoredVersionNumber(stored)
	if err != nil {
		return 0
	}
	return version
}

// Fingerprint identifies the current, live documents by path, version and
// content hash, in any indexing order; every write, archive or unarchive
// changes it. Empty until BuildIndex.
func (s *Store) Fingerprint() string {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	if !s.indexed {
		return ""
	}
	return hex.EncodeToString(s.digest[:])
}
