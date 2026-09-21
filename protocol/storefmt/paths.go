package storefmt

import (
	"io/fs"
	"path"
	"strings"
)

// CanonicalPath is the one spelling of a request path shared by every index
// keyed on paths (hash index, catalog): slash-cleaned, single leading
// slash, root is "/". Callers reject traversal via ContainsDotDot.
func CanonicalPath(reqPath string) string {
	return path.Clean("/" + reqPath)
}

// RelPath is the storage-relative form of a request path: CanonicalPath
// without the leading slash, "" for the root. ".." is rejected with
// fs.ErrNotExist so traversal never reaches a backend.
func RelPath(reqPath string) (string, error) {
	if ContainsDotDot(reqPath) {
		return "", fs.ErrNotExist
	}
	return strings.TrimPrefix(CanonicalPath(reqPath), "/"), nil
}

// ContainsDotDot reports whether the path contains a ".." segment. Every
// backend rejects such paths with fs.ErrNotExist; exported so they share one
// definition of traversal.
func ContainsDotDot(reqPath string) bool {
	for seg := range strings.SplitSeq(reqPath, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
