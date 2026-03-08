package graphstore

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// nodeRowRe matches a node table row: | [url](url) | title | status | N |
var nodeRowRe = regexp.MustCompile(`^\|\s*\[([^\]]+)\]\([^)]+\)\s*\|\s*(.*?)\s*\|\s*(\S+)\s*\|\s*(\d+)\s*\|$`)

// edgeRowRe matches an edge table row: | from | to |
var edgeRowRe = regexp.MustCompile(`^\|\s*(\S+)\s*\|\s*(\S+)\s*\|$`)

// Export renders the graph store as a publishable markdown document.
// The output contains mark:// links so crawling the document naturally
// discovers the topology. Thread-safe.
func (s *Store) Export() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Sort nodes by URL for stable output.
	nodes := make([]StoredNode, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, *n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].URL < nodes[j].URL
	})

	// Sort edges by From, then To.
	edges := make([]StoredEdge, len(s.edges))
	copy(edges, s.edges)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})

	var b strings.Builder
	b.WriteString("# Document Graph\n\n")
	b.WriteString(fmt.Sprintf("> Exported: %s\n", time.Now().UTC().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("> Nodes: %d\n", len(nodes)))
	b.WriteString(fmt.Sprintf("> Edges: %d\n", len(edges)))

	b.WriteString("\n## Nodes\n\n")
	b.WriteString("| URL | Title | Status | Links |\n")
	b.WriteString("|-----|-------|--------|-------|\n")
	for _, n := range nodes {
		b.WriteString(fmt.Sprintf("| [%s](%s) | %s | %s | %d |\n",
			n.URL, n.URL, n.Title, n.Status, n.LinkCount))
	}

	if len(edges) > 0 {
		b.WriteString("\n## Edges\n\n")
		b.WriteString("| From | To |\n")
		b.WriteString("|------|----|")
		for _, e := range edges {
			b.WriteString(fmt.Sprintf("\n| %s | %s |", e.From, e.To))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// ParseExport extracts nodes and edges from an exported graph markdown document.
// It follows the same table-parsing pattern as the index package.
func ParseExport(body string) ([]StoredNode, []StoredEdge) {
	var nodes []StoredNode
	var edges []StoredEdge

	inTable := false
	headerSeen := false
	section := "" // "nodes" or "edges"

	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)

		// Detect section headers.
		if strings.HasPrefix(trimmed, "## Nodes") {
			section = "nodes"
			inTable = false
			headerSeen = false
			continue
		}
		if strings.HasPrefix(trimmed, "## Edges") {
			section = "edges"
			inTable = false
			headerSeen = false
			continue
		}

		if !strings.HasPrefix(trimmed, "|") {
			inTable = false
			headerSeen = false
			continue
		}

		if !inTable {
			inTable = true
			headerSeen = false
			continue
		}
		if !headerSeen {
			headerSeen = true
			continue
		}

		switch section {
		case "nodes":
			m := nodeRowRe.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			linkCount, _ := strconv.Atoi(m[4])
			nodes = append(nodes, StoredNode{
				URL:       m[1],
				Title:     m[2],
				Status:    m[3],
				LinkCount: linkCount,
			})
		case "edges":
			m := edgeRowRe.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			edges = append(edges, StoredEdge{From: m[1], To: m[2]})
		}
	}

	return nodes, edges
}
