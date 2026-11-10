package graph

import (
	"net/url"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// IsGeneratedURL reports a canonical mark:// URL, or the rooted path a
// single store names a document by, of generated graph data: neither node
// nor edge end (SPEC 12.1). It runs per stored row.
func IsGeneratedURL(raw string) bool {
	docPath := raw
	if rest, ok := strings.CutPrefix(raw, "mark://"); ok {
		start := strings.IndexByte(rest, '/')
		if start < 0 {
			return false
		}
		docPath = rest[start:]
	} else if !strings.HasPrefix(raw, "/") {
		return false
	}
	// A generated path decodes to /graph...: two bytes settle the rest.
	if len(docPath) < 2 || (docPath[1] != 'g' && docPath[1] != '%') {
		return false
	}
	if strings.IndexByte(docPath, '%') >= 0 {
		unescaped, err := url.PathUnescape(docPath)
		if err != nil {
			return false
		}
		docPath = unescaped
	}
	return protocol.IsGeneratedGraphPath(docPath)
}
