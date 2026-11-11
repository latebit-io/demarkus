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
package store

import (
	"sync"
)

// Store is one versioned document directory: the files, the index of its
// current documents and their fingerprint. One process writes it at a time;
// another marks its writes (MarkChanged) so the first indexes again.
type Store struct {
	root    string
	indexMu sync.RWMutex
	// pathIdx: canonical request path → its fold, the digest contribution it
	// is removed with. liveChildren also reads membership as "current,
	// non-archived doc" for ListDir filtering, so keep membership exact.
	pathIdx map[string][digestLen]byte
	// digest is the XOR of every entry's fold, so it updates per entry and
	// equals any other index over the same current documents. Meaningful
	// only once indexed is set by BuildIndex. indexMu.
	digest  [digestLen]byte
	indexed bool
	dirDebt dirSyncDebt
	// cleanDirs: versions dirs proven free of flat-layout files, so a write
	// pays the scan once per directory. Live set, one entry per directory.
	cleanMu   sync.Mutex
	cleanDirs map[string]struct{}
}

// New creates a store rooted at the given directory.
func New(root string) *Store {
	return &Store{
		root:      root,
		pathIdx:   make(map[string][digestLen]byte),
		cleanDirs: make(map[string]struct{}),
	}
}

// Root returns the content directory path.
func (s *Store) Root() string {
	return s.root
}

// VersionFilePath returns where version N of reqPath lives on disk, for
// tools and tests that touch the layout directly. It does not check that
// the file exists or follow symlinks.
func (s *Store) VersionFilePath(reqPath string, version int) (string, error) {
	loc, err := s.locateContained(reqPath)
	if err != nil {
		return "", err
	}
	return loc.versionFile(version), nil
}
