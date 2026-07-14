// Package graph provides a document graph data structure and crawler for
// discovering link relationships between Mark Protocol documents.
package graph

import (
	"strconv"
	"strings"
	"sync"
)

// Node represents a document in the graph.
type Node struct {
	URL       string
	Title     string
	Depth     int
	Status    string // protocol status (e.g. "ok", "not-found"), "error", "external", or "" for undiscovered
	LinkCount int
}

// Edge represents a directed link from one document to another.
// Identity is {From, To, Rel}: a plain body link (Rel "") and a typed
// rel-<predicate> edge between the same pair are distinct edges.
type Edge struct {
	From   string
	To     string
	Rel    string // typed-relation predicate ("" for plain body links)
	Label  string // link label text of the first labeled occurrence
	Anchor string // source section anchor of that occurrence (no '#')
	Count  int    // occurrences aggregated into this edge (>= 1)
}

// Annotation renders the edge's provenance suffix for display.
func (e *Edge) Annotation() string {
	return EdgeAnnotation(e.Rel, e.Label, e.Anchor, e.Count)
}

// EdgeAnnotation renders the terse provenance suffix shared by every edge
// formatter (client MCP, broker MCP, backlink lists). Empty for a plain
// single-occurrence edge.
func EdgeAnnotation(rel, label, anchor string, count int) string {
	var parts []string
	if label != "" {
		parts = append(parts, strconv.Quote(label))
	}
	if anchor != "" {
		parts = append(parts, "#"+anchor)
	}
	if count > 1 {
		parts = append(parts, "x"+strconv.Itoa(count))
	}
	s := ""
	if rel != "" {
		s = " [" + rel + "]"
	}
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

// edgeKey is the identity of an edge for deduplication.
type edgeKey struct{ from, to, rel string }

// Graph is a concurrency-safe directed graph of document nodes and link edges.
type Graph struct {
	nodes   map[string]*Node
	edges   []Edge
	edgeIdx map[edgeKey]int
	mu      sync.RWMutex
}

// New creates an empty graph.
func New() *Graph {
	return &Graph{
		nodes:   make(map[string]*Node),
		edgeIdx: make(map[edgeKey]int),
	}
}

// AddNode adds or replaces a node in the graph.
func (g *Graph) AddNode(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.URL] = n
}

// AddEdge adds a plain (untyped, unlabeled) directed edge.
// Occurrences of the same edge aggregate into its Count.
func (g *Graph) AddEdge(from, to string) {
	g.AddEdgeInfo(Edge{From: from, To: to})
}

// AddEdgeInfo adds or aggregates an edge keyed on {From, To, Rel}.
// Count is normalized to at least 1; duplicates add their Count, and the
// first occurrence with a non-empty Label supplies Label and Anchor.
func (g *Graph) AddEdgeInfo(e Edge) { //nolint:gocritic // hugeParam: by value is intentional; the copy is normalized locally before insert
	if e.Count < 1 {
		e.Count = 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := edgeKey{from: e.From, to: e.To, rel: e.Rel}
	i, exists := g.edgeIdx[key]
	if !exists {
		g.edgeIdx[key] = len(g.edges)
		g.edges = append(g.edges, e)
		return
	}
	cur := &g.edges[i]
	cur.Count += e.Count
	if cur.Label == "" && e.Label != "" {
		cur.Label, cur.Anchor = e.Label, e.Anchor
	} else if cur.Label == "" && cur.Anchor == "" && e.Anchor != "" {
		cur.Anchor = e.Anchor
	}
}

// GetNode returns the node for a URL, or nil if not found.
func (g *Graph) GetNode(url string) *Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.nodes[url]
}

// Neighbors returns all nodes that the given URL links to.
func (g *Graph) Neighbors(url string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var result []*Node
	for _, e := range g.edges {
		if e.From == url {
			if n, ok := g.nodes[e.To]; ok {
				result = append(result, n)
			}
		}
	}
	return result
}

// InNeighbors returns all nodes that link TO the given URL (reverse edges).
func (g *Graph) InNeighbors(url string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var result []*Node
	for _, e := range g.edges {
		if e.To == url {
			if n, ok := g.nodes[e.From]; ok {
				result = append(result, n)
			}
		}
	}
	return result
}

// InDegrees returns a map of URL to inbound edge count for all nodes.
// Single O(E) pass, more efficient than calling InNeighbors per node.
func (g *Graph) InDegrees() map[string]int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	counts := make(map[string]int, len(g.nodes))
	for url := range g.nodes {
		counts[url] = 0
	}
	for _, e := range g.edges {
		if _, ok := g.nodes[e.To]; ok {
			counts[e.To]++
		}
	}
	return counts
}

// NodeCount returns the number of nodes in the graph.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// GetEdges returns a copy of the edge list.
func (g *Graph) GetEdges() []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	edges := make([]Edge, len(g.edges))
	copy(edges, g.edges)
	return edges
}

// EdgeCount returns the number of edges in the graph.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.edges)
}

// AllNodes returns a copy of all nodes in the graph.
func (g *Graph) AllNodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	nodes := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		nodes = append(nodes, n)
	}
	return nodes
}
