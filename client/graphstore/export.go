package graphstore

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// nodeRowRe matches a node table row: | [label](url) | title | status | N |
// Captures the link destination (not the display text) as the canonical URL.
// The title group handles backslash-escaped pipes (\|) so they aren't treated as column delimiters.
var nodeRowRe = regexp.MustCompile(`^\|\s*\[[^\]]+\]\(([^)]+)\)\s*\|\s*((?:[^\\|]|\\.)*?)\s*\|\s*(\S+)\s*\|\s*(\d+)\s*\|$`)

// edgeRowRe matches an enriched edge row: | from | to | rel | label | anchor | count |
// Rel and anchor may be empty; the label group handles escaped pipes like nodeRowRe's title.
var edgeRowRe = regexp.MustCompile(`^\|\s*(\S+)\s*\|\s*(\S+)\s*\|\s*(\S*)\s*\|\s*((?:[^\\|]|\\.)*?)\s*\|\s*(\S*)\s*\|\s*(\d+)\s*\|$`)

// legacyEdgeRowRe matches the pre-enrichment two-column row: | from | to |
// Published /graph.md documents in the wild still carry this shape.
var legacyEdgeRowRe = regexp.MustCompile(`^\|\s*(\S+)\s*\|\s*(\S+)\s*\|$`)

// Export renders the graph store as a publishable markdown document.
// The output contains mark:// links so crawling the document naturally
// discovers the topology. Thread-safe.
func (s *Store) Export() string {
	nodes, edges := s.Snapshot()
	return BuildExport(time.Now(), nodes, edges)
}

// Snapshot returns deterministic copies of the graph's nodes and edges.
func (s *Store) Snapshot() ([]StoredNode, []StoredEdge) {
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
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Rel < edges[j].Rel
	})
	return nodes, edges
}

// BuildExport renders a deterministic graph export for one timestamp.
func BuildExport(exported time.Time, nodes []StoredNode, edges []StoredEdge) string {
	nodes = append([]StoredNode(nil), nodes...)
	edges = append([]StoredEdge(nil), edges...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].URL < nodes[j].URL })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Rel < edges[j].Rel
	})
	var b strings.Builder
	b.WriteString("# Document Graph\n\n")
	b.WriteString(fmt.Sprintf("> Exported: %s\n", exported.UTC().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("> Nodes: %d\n", len(nodes)))
	b.WriteString(fmt.Sprintf("> Edges: %d\n", len(edges)))

	b.WriteString("\n## Nodes\n\n")
	b.WriteString("| URL | Title | Status | Links |\n")
	b.WriteString("|-----|-------|--------|-------|\n")
	for _, n := range nodes {
		b.WriteString(fmt.Sprintf("| [%s](%s) | %s | %s | %d |\n",
			n.URL, n.URL, escapeCell(n.Title), n.Status, n.LinkCount))
	}

	if len(edges) > 0 {
		b.WriteString("\n## Edges\n\n")
		b.WriteString("| From | To | Rel | Label | Anchor | Count |\n")
		b.WriteString("|------|----|-----|-------|--------|-------|")
		for _, e := range edges {
			b.WriteString(fmt.Sprintf("\n| %s | %s | %s | %s | %s | %d |",
				e.From, e.To, e.Rel, escapeCell(e.Label), e.Anchor, max(e.Count, 1)))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// escapeCell escapes characters that would break markdown table formatting.
// Backslashes are escaped first so that subsequent escapes are unambiguous.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// unescapeCell reverses escapeCell in reverse order.
func unescapeCell(s string) string {
	s = strings.ReplaceAll(s, `\|`, "|")
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
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
				Title:     unescapeCell(m[2]),
				Status:    m[3],
				LinkCount: linkCount,
			})
		case "edges":
			// The two shapes are $-anchored with disjoint column counts,
			// so they cannot cross-match; enriched first for clarity.
			if m := edgeRowRe.FindStringSubmatch(trimmed); m != nil {
				count, _ := strconv.Atoi(m[6])
				edges = append(edges, StoredEdge{
					From:   m[1],
					To:     m[2],
					Rel:    m[3],
					Label:  unescapeCell(m[4]),
					Anchor: m[5],
					Count:  max(count, 1),
				})
				continue
			}
			if m := legacyEdgeRowRe.FindStringSubmatch(trimmed); m != nil {
				edges = append(edges, StoredEdge{From: m[1], To: m[2], Count: 1})
			}
		}
	}

	return nodes, edges
}

// ParseExportStrict validates a generated legacy export before parsing it.
func ParseExportStrict(body string) ([]StoredNode, []StoredEdge, error) {
	if len(body) > protocol.MaxBodyLength {
		return nil, nil, fmt.Errorf("graph export exceeds body limit: %d", len(body))
	}
	if firstNonemptyExportLine(body) != "# Document Graph" {
		return nil, nil, errors.New("graph export title is invalid")
	}
	meta := make(map[string]string)
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "> ") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "> "), ": ")
		if !ok || value == "" {
			return nil, nil, fmt.Errorf("invalid graph export metadata %q", line)
		}
		if key != "Exported" && key != "Nodes" && key != "Edges" {
			return nil, nil, fmt.Errorf("unknown graph export metadata %q", key)
		}
		if _, duplicate := meta[key]; duplicate {
			return nil, nil, fmt.Errorf("duplicate graph export metadata %q", key)
		}
		meta[key] = value
	}
	exported, err := time.Parse(time.RFC3339, meta["Exported"])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid graph export time: %w", err)
	}
	if exported.Year() < 1 || exported.Year() > 9999 {
		return nil, nil, errors.New("invalid graph export year")
	}
	declaredNodes, err := strconv.Atoi(meta["Nodes"])
	if err != nil || declaredNodes < 0 {
		return nil, nil, errors.New("invalid graph export node count")
	}
	declaredEdges, err := strconv.Atoi(meta["Edges"])
	if err != nil || declaredEdges < 0 {
		return nil, nil, errors.New("invalid graph export edge count")
	}
	if err := validateExportTables(body, declaredEdges > 0); err != nil {
		return nil, nil, err
	}
	nodes, edges := ParseExport(body)
	if len(nodes) != declaredNodes || len(edges) != declaredEdges {
		return nil, nil, fmt.Errorf("graph export count mismatch: got %d nodes/%d edges, want %d/%d",
			len(nodes), len(edges), declaredNodes, declaredEdges)
	}
	if err := normalizeStrictExport(nodes, edges); err != nil {
		return nil, nil, err
	}
	return nodes, edges, nil
}

func normalizeStrictExport(nodes []StoredNode, edges []StoredEdge) error {
	seenNodes := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		if err := validateSnapshotURL(nodes[i].URL); err != nil {
			return err
		}
		nodes[i].URL = links.CanonicalURL(nodes[i].URL)
		if _, duplicate := seenNodes[nodes[i].URL]; duplicate {
			return fmt.Errorf("duplicate graph node %q", nodes[i].URL)
		}
		seenNodes[nodes[i].URL] = struct{}{}
	}
	seenEdges := make(map[edgeKey]struct{}, len(edges))
	for i := range edges {
		if err := validateSnapshotURL(edges[i].From); err != nil {
			return err
		}
		if err := validateSnapshotURL(edges[i].To); err != nil {
			return err
		}
		edges[i].From = links.CanonicalURL(edges[i].From)
		edges[i].To = links.CanonicalURL(edges[i].To)
		key := edges[i].key()
		if _, duplicate := seenEdges[key]; duplicate {
			return fmt.Errorf("duplicate graph edge %q -> %q", edges[i].From, edges[i].To)
		}
		seenEdges[key] = struct{}{}
	}
	return nil
}

func validateExportTables(body string, requireEdges bool) error { //nolint:gocyclo // strict state-machine validation stays linear
	section := ""
	rowIndex := 0
	edgeColumns := 6
	foundNodes, foundEdges := false, false
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "## Nodes":
			if foundNodes {
				return errors.New("duplicate graph nodes table")
			}
			section, rowIndex, foundNodes = "nodes", 0, true
			continue
		case "## Edges":
			if foundEdges {
				return errors.New("duplicate graph edges table")
			}
			if section == "nodes" && rowIndex < 2 {
				return errors.New("graph nodes table is incomplete")
			}
			section, rowIndex, foundEdges = "edges", 0, true
			continue
		}
		if section == "" || !strings.HasPrefix(trimmed, "|") {
			continue
		}
		rowIndex++
		if rowIndex == 1 {
			want := "| URL | Title | Status | Links |"
			if section == "edges" {
				want = "| From | To | Rel | Label | Anchor | Count |"
				if trimmed == "| From | To |" {
					want, edgeColumns = trimmed, 2
				}
			}
			if trimmed != want {
				return fmt.Errorf("invalid graph %s header", section)
			}
			continue
		}
		if rowIndex == 2 {
			cells := strings.Split(strings.Trim(trimmed, "|"), "|")
			wantColumns := 4
			if section == "edges" {
				wantColumns = edgeColumns
			}
			if len(cells) != wantColumns {
				return fmt.Errorf("invalid graph %s separator", section)
			}
			for _, cell := range cells {
				cell = strings.TrimSpace(cell)
				if len(cell) < 3 || strings.Trim(cell, "-") != "" {
					return fmt.Errorf("invalid graph %s separator", section)
				}
			}
			continue
		}
		if section == "nodes" && nodeRowRe.FindStringSubmatch(trimmed) == nil {
			return fmt.Errorf("invalid graph node row %q", trimmed)
		}
		if section == "nodes" {
			match := nodeRowRe.FindStringSubmatch(trimmed)
			if _, err := strconv.Atoi(match[4]); err != nil {
				return fmt.Errorf("invalid graph node link count %q", match[4])
			}
		}
		if section == "edges" && ((edgeColumns == 6 && edgeRowRe.FindStringSubmatch(trimmed) == nil) ||
			(edgeColumns == 2 && legacyEdgeRowRe.FindStringSubmatch(trimmed) == nil)) {
			return fmt.Errorf("invalid graph edge row %q", trimmed)
		}
		if section == "edges" && edgeColumns == 6 {
			match := edgeRowRe.FindStringSubmatch(trimmed)
			count, err := strconv.Atoi(match[6])
			if err != nil || count < 1 {
				return fmt.Errorf("invalid graph edge count %q", match[6])
			}
		}
	}
	if section != "" && rowIndex < 2 {
		return fmt.Errorf("graph %s table is incomplete", section)
	}
	if !foundNodes || (requireEdges && !foundEdges) {
		return errors.New("graph export table is missing")
	}
	return nil
}

func firstNonemptyExportLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
