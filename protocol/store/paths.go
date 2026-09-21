package store

import (
	"fmt"
	"path"
	"path/filepath"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// versionsDirName is the directory beside a document that holds its history.
const versionsDirName = "versions"

// docLocation is where one document lives under the root. It is derived from
// the request path alone; containment is a separate check (locateContained).
type docLocation struct {
	rel         string // slash path without the leading slash
	base        string
	current     string // the current pointer: <root>/<dir>/<base>
	versionsDir string // <root>/<dir>/versions
	docDir      string // <versionsDir>/<base>
}

// locate derives a document's location. The request side uses slash paths;
// the one conversion to the OS separator happens here.
func (s *Store) locate(reqPath string) (docLocation, error) {
	rel, err := storefmt.RelPath(reqPath)
	if err != nil {
		return docLocation{}, err
	}
	dir := filepath.Join(s.root, filepath.FromSlash(path.Dir(rel)))
	base := path.Base(rel)
	versionsDir := filepath.Join(dir, versionsDirName)
	return docLocation{
		rel:         rel,
		base:        base,
		current:     filepath.Join(dir, base),
		versionsDir: versionsDir,
		docDir:      filepath.Join(versionsDir, base),
	}, nil
}

// locateContained is locate plus the symlink check on the versions tree.
func (s *Store) locateContained(reqPath string) (docLocation, error) {
	loc, err := s.locate(reqPath)
	if err != nil {
		return docLocation{}, err
	}
	if _, err := s.contain(loc.docDir); err != nil {
		return docLocation{}, err
	}
	return loc, nil
}

// versionFile is the on-disk path of one version.
func (loc *docLocation) versionFile(version int) string {
	return newVersionFilePath(loc.versionsDir, loc.base, version)
}

// versionReqPath is the same file as a request path, for resolve.
func (loc *docLocation) versionReqPath(version int) string {
	return "/" + path.Join(path.Dir(loc.rel), versionsDirName, loc.base, fmt.Sprintf("v%d", version))
}
