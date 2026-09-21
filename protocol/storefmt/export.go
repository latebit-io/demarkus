package storefmt

import (
	"bytes"
	"context"
	"fmt"
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

// ValidateImport checks the shared ImportDoc preconditions: a document path, at
// least one version, versions strictly ascending (a pruned history may start
// past 1), none oversized.
func ValidateImport(reqPath string, document StoredDocument) error {
	rel, err := RelPath(reqPath)
	if err != nil {
		return err
	}
	if rel == "" || strings.HasSuffix(reqPath, "/") {
		return fmt.Errorf("import %s: not a document path", reqPath)
	}
	if len(document.Versions) == 0 {
		return fmt.Errorf("import %s: no versions", reqPath)
	}
	for i, v := range document.Versions {
		if i > 0 && v.Version <= document.Versions[i-1].Version {
			return fmt.Errorf("import %s: versions not strictly ascending", reqPath)
		}
		if int64(len(v.Stored)) > int64(protocol.MaxBodyLength+MaxStoreFrontmatter) {
			return fmt.Errorf("import %s v%d: %w", reqPath, v.Version, ErrSizeLimit)
		}
	}
	return nil
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
