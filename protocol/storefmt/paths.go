package storefmt

import (
	"io/fs"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// CanonicalPath is the one spelling of a request path shared by every index
// keyed on paths (document index, catalog): slash-cleaned, single leading
// slash, root is "/". Callers reject traversal via ContainsDotDot.
func CanonicalPath(reqPath string) string {
	return path.Clean("/" + reqPath)
}

// ValidDocumentPath reports a path a document may live at: a valid request
// path, in its canonical spelling, below the root. Being one, it fits a
// stored frontmatter line within MaxStoreFrontmatter.
func ValidDocumentPath(p string) bool {
	return p != "/" && protocol.ValidateRequestPath(p) == nil && CanonicalPath(p) == p
}

// IsGovernancePath reports a path under protocol.GovernancePrefix, in its
// canonical spelling and case-folded: a case-folding disk maps a variant onto
// the same file.
func IsGovernancePath(reqPath string) bool {
	return strings.HasPrefix(strings.ToLower(CanonicalPath(reqPath))+"/", protocol.GovernancePrefix)
}

// ScopePath is the canonical path with a trailing slash kept: "/" or a
// prefix ending in "/" names a subtree, anything else one document.
func ScopePath(reqPath string) string {
	scope := CanonicalPath(reqPath)
	if strings.HasSuffix(reqPath, "/") && scope != "/" {
		scope += "/"
	}
	return scope
}

// GeneratedGraphTarget reports a write target that is generated graph data,
// or a directory of it, under any spelling: scoped and case-folded, where
// protocol.IsGeneratedGraphPath takes a stored path.
func GeneratedGraphTarget(reqPath string) bool {
	return protocol.IsGeneratedGraphPath(strings.ToLower(ScopePath(reqPath)))
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
