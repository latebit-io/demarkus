package protocol

import "testing"

func TestIsGeneratedGraphPath(t *testing.T) {
	for _, path := range []string{
		"/graph.md", "/graph.md/v3",
		"/graph/manifest.md", "/graph/manifest.md/v12",
		"/graph/shards/a/nodes-000.md", "/graph/shards/b/edges-001.md/v2",
		"/graph/worlds/alpha/manifest.md", "/graph/worlds/alpha/0f.md",
		"/graph/shards/", "/graph/worlds/alpha/",
	} {
		if !IsGeneratedGraphPath(path) {
			t.Errorf("%q should be generated graph data", path)
		}
	}
	for _, path := range []string{
		"", "/", "/graph", "/graph/", "/graph/notes.md", "/graph.md/v0", "/graph.md/vx", "/graph.md/notes.md",
		"/graph/shards", "/graph/worlds", "/graph/worlds.md", "/graphs/x.md", "/docs/graph.md", "/docs/graph/manifest.md",
	} {
		if IsGeneratedGraphPath(path) {
			t.Errorf("%q should not be generated graph data", path)
		}
	}
}
