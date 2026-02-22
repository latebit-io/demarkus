package graph

import (
	"testing"
)

func TestNewGraph(t *testing.T) {
	g := New()
	if g.NodeCount() != 0 {
		t.Errorf("NodeCount() = %d, want 0", g.NodeCount())
	}
	if g.EdgeCount() != 0 {
		t.Errorf("EdgeCount() = %d, want 0", g.EdgeCount())
	}
}

func TestAddNode(t *testing.T) {
	g := New()
	g.AddNode(&Node{URL: "mark://host/a.md", Title: "A", Depth: 0, Status: "ok"})

	if g.NodeCount() != 1 {
		t.Fatalf("NodeCount() = %d, want 1", g.NodeCount())
	}

	n := g.GetNode("mark://host/a.md")
	if n == nil {
		t.Fatal("GetNode returned nil")
	}
	if n.Title != "A" {
		t.Errorf("Title = %q, want %q", n.Title, "A")
	}
}

func TestAddNodeUpdatesExisting(t *testing.T) {
	g := New()
	g.AddNode(&Node{URL: "mark://host/a.md", Title: "Old"})
	g.AddNode(&Node{URL: "mark://host/a.md", Title: "New"})

	if g.NodeCount() != 1 {
		t.Fatalf("NodeCount() = %d, want 1", g.NodeCount())
	}
	if g.GetNode("mark://host/a.md").Title != "New" {
		t.Error("node was not updated")
	}
}

func TestGetNodeNotFound(t *testing.T) {
	g := New()
	if g.GetNode("mark://host/nope.md") != nil {
		t.Error("expected nil for missing node")
	}
}

func TestAddEdge(t *testing.T) {
	g := New()
	g.AddNode(&Node{URL: "mark://host/a.md"})
	g.AddNode(&Node{URL: "mark://host/b.md"})
	g.AddEdge("mark://host/a.md", "mark://host/b.md")

	if g.EdgeCount() != 1 {
		t.Fatalf("EdgeCount() = %d, want 1", g.EdgeCount())
	}
}

func TestAddEdgeIgnoresDuplicates(t *testing.T) {
	g := New()
	g.AddNode(&Node{URL: "mark://host/a.md"})
	g.AddNode(&Node{URL: "mark://host/b.md"})
	g.AddEdge("mark://host/a.md", "mark://host/b.md")
	g.AddEdge("mark://host/a.md", "mark://host/b.md")
	g.AddEdge("mark://host/a.md", "mark://host/b.md")

	if g.EdgeCount() != 1 {
		t.Fatalf("EdgeCount() = %d, want 1", g.EdgeCount())
	}
}

func TestNeighbors(t *testing.T) {
	g := New()
	g.AddNode(&Node{URL: "mark://host/a.md", Title: "A"})
	g.AddNode(&Node{URL: "mark://host/b.md", Title: "B"})
	g.AddNode(&Node{URL: "mark://host/c.md", Title: "C"})
	g.AddEdge("mark://host/a.md", "mark://host/b.md")
	g.AddEdge("mark://host/a.md", "mark://host/c.md")
	g.AddEdge("mark://host/b.md", "mark://host/c.md")

	neighbors := g.Neighbors("mark://host/a.md")
	if len(neighbors) != 2 {
		t.Fatalf("Neighbors(a) = %d nodes, want 2", len(neighbors))
	}

	// b should only have c as neighbor
	neighbors = g.Neighbors("mark://host/b.md")
	if len(neighbors) != 1 {
		t.Fatalf("Neighbors(b) = %d nodes, want 1", len(neighbors))
	}
	if neighbors[0].Title != "C" {
		t.Errorf("Neighbors(b)[0].Title = %q, want %q", neighbors[0].Title, "C")
	}

	// c has no outbound edges
	neighbors = g.Neighbors("mark://host/c.md")
	if len(neighbors) != 0 {
		t.Fatalf("Neighbors(c) = %d nodes, want 0", len(neighbors))
	}
}

func TestAllNodes(t *testing.T) {
	g := New()
	if len(g.AllNodes()) != 0 {
		t.Errorf("AllNodes() on empty graph = %d, want 0", len(g.AllNodes()))
	}

	g.AddNode(&Node{URL: "mark://host/a.md", Title: "A"})
	g.AddNode(&Node{URL: "mark://host/b.md", Title: "B"})
	g.AddNode(&Node{URL: "mark://host/c.md", Title: "C"})

	nodes := g.AllNodes()
	if len(nodes) != 3 {
		t.Fatalf("AllNodes() = %d, want 3", len(nodes))
	}

	urls := make(map[string]bool)
	for _, n := range nodes {
		urls[n.URL] = true
	}
	for _, url := range []string{"mark://host/a.md", "mark://host/b.md", "mark://host/c.md"} {
		if !urls[url] {
			t.Errorf("AllNodes() missing %q", url)
		}
	}
}
