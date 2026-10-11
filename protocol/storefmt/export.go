package storefmt

import (
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

// ValidateExport checks an exported document: a document path, at least one
// version, versions strictly ascending (a pruned history may start past 1),
// none oversized.
func ValidateExport(reqPath string, document StoredDocument) error {
	rel, err := RelPath(reqPath)
	if err != nil {
		return err
	}
	if rel == "" || strings.HasSuffix(reqPath, "/") {
		return fmt.Errorf("export %s: not a document path", reqPath)
	}
	if len(document.Versions) == 0 {
		return fmt.Errorf("export %s: no versions", reqPath)
	}
	for i, v := range document.Versions {
		if i > 0 && v.Version <= document.Versions[i-1].Version {
			return fmt.Errorf("export %s: versions not strictly ascending", reqPath)
		}
		if int64(len(v.Stored)) > int64(protocol.MaxBodyLength+MaxStoreFrontmatter) {
			return fmt.Errorf("export %s v%d: %w", reqPath, v.Version, ErrSizeLimit)
		}
	}
	return nil
}
