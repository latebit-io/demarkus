// Package graph provides a document graph data structure and crawler for
// discovering link relationships between Mark Protocol documents.
package graph

import "sync"

// Node represents a document in the graph.
type Node struct {
	URL       string
	Title     string
	Depth     int
	Status    string // protocol status (e.g. "ok", "not-found"), "error", "external", or "" for undiscovered
	LinkCount int
}

// Edge represents a directed link from one document to another.
type Edge struct {
	From string
	To   string
}

// Graph is a concurrency-safe directed graph of document nodes and link edges.
type Graph struct {
	nodes   map[string]*Node
	edges   []Edge
	edgeSet map[Edge]struct{}
	mu      sync.RWMutex
}

// New creates an empty graph.
func New() *Graph {
	return &Graph{
		nodes:   make(map[string]*Node),
		edgeSet: make(map[Edge]struct{}),
	}
}

// AddNode adds or replaces a node in the graph.
func (g *Graph) AddNode(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.URL] = n
}

// AddEdge adds a directed edge from one URL to another.
// Duplicate edges are ignored.
func (g *Graph) AddEdge(from, to string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := Edge{From: from, To: to}
	if _, exists := g.edgeSet[e]; exists {
		return
	}
	g.edgeSet[e] = struct{}{}
	g.edges = append(g.edges, e)
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
