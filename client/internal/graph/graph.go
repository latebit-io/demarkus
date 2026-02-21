// Package graph provides a document graph data structure and crawler for
// discovering link relationships between Mark Protocol documents.
package graph

import "sync"

// Node represents a document in the graph.
type Node struct {
	URL       string
	Title     string
	Depth     int
	Status    string // "ok", "not-found", "error", or "" for undiscovered
	LinkCount int
}

// Edge represents a directed link from one document to another.
type Edge struct {
	From string
	To   string
}

// Graph is a concurrency-safe directed graph of document nodes and link edges.
type Graph struct {
	Nodes   map[string]*Node
	Edges   []Edge
	edgeSet map[Edge]struct{}
	mu      sync.RWMutex
}

// New creates an empty graph.
func New() *Graph {
	return &Graph{
		Nodes:   make(map[string]*Node),
		edgeSet: make(map[Edge]struct{}),
	}
}

// AddNode adds or updates a node in the graph.
// If the node already exists, it is updated in place.
func (g *Graph) AddNode(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Nodes[n.URL] = n
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
	g.Edges = append(g.Edges, e)
}

// GetNode returns the node for a URL, or nil if not found.
func (g *Graph) GetNode(url string) *Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.Nodes[url]
}

// Neighbors returns all nodes that the given URL links to.
func (g *Graph) Neighbors(url string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var result []*Node
	for _, e := range g.Edges {
		if e.From == url {
			if n, ok := g.Nodes[e.To]; ok {
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
	return len(g.Nodes)
}

// EdgeCount returns the number of edges in the graph.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.Edges)
}
