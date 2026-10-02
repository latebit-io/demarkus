package graphstore

import (
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
)

// BuildWorldsExport renders checkpoint rows, by world name, as the legacy
// export: an ok node per row and its edges, with no source observations, so
// only the rows and exported change the body.
func BuildWorldsExport(exported time.Time, worlds map[string][]WorldSource) string {
	rows, links := 0, 0
	for _, sources := range worlds {
		rows += len(sources)
		for i := range sources {
			links += len(sources[i].Edges)
		}
	}
	nodes, edges := make([]StoredNode, 0, rows), make([]StoredEdge, 0, links)
	for world, sources := range worlds {
		worldNodes, worldEdges := worldRows(world, sources, time.Time{})
		for i := range worldNodes {
			worldNodes[i].Observation = graph.Observation{}
		}
		nodes, edges = append(nodes, worldNodes...), append(edges, worldEdges...)
	}
	return buildExport(exported, nodes, edges)
}

// ExportContent is an export without its Exported line: two renders of the
// same graph share it.
func ExportContent(body string) string {
	start := strings.Index(body, "\n"+exportedLine)
	if start < 0 {
		return body
	}
	end := strings.IndexByte(body[start+1:], '\n')
	if end < 0 {
		return body[:start]
	}
	return body[:start] + body[start+1+end:]
}
