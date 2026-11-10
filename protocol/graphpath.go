package protocol

import (
	"strconv"
	"strings"
)

// Paths of the graph data a world's graph publishers write (§12.1). They are
// generated, not authored: LOOKUP omits them and crawlers read no links there.
const (
	// GraphExportPath is the single-document graph export.
	GraphExportPath = "/graph.md"
	// GraphSnapshotManifestPath is the entry point of a sharded graph snapshot.
	GraphSnapshotManifestPath = "/graph/manifest.md"
	// GraphSnapshotShardRoot holds a snapshot's shards.
	GraphSnapshotShardRoot = "/graph/shards"
	// GraphWorldRoot holds one directory of checkpoint documents per world.
	GraphWorldRoot = "/graph/worlds"
)

// IsGeneratedGraphPath reports whether docPath, or a version of it, is
// generated graph data rather than an authored document; a directory that
// holds only such data answers too, given its trailing slash.
func IsGeneratedGraphPath(docPath string) bool {
	return isPathOrVersion(docPath, GraphExportPath) || isPathOrVersion(docPath, GraphSnapshotManifestPath) ||
		strings.HasPrefix(docPath, GraphSnapshotShardRoot+"/") || strings.HasPrefix(docPath, GraphWorldRoot+"/")
}

// isPathOrVersion reports whether docPath is base or base/vN.
func isPathOrVersion(docPath, base string) bool {
	rest, ok := strings.CutPrefix(docPath, base)
	if !ok || rest == "" {
		return ok
	}
	version, ok := strings.CutPrefix(rest, "/v")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(version)
	return err == nil && n > 0
}
