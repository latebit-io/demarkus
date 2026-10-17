package graphstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestLoadsUpgradeCapture loads the graph.json a released demarkus-mcp wrote
// (testdata/upgrade, scripts/capture-upgrade-fixture.sh). The store is a
// cache: an older schema loads empty and refills on the next crawl.
func TestLoadsUpgradeCapture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "upgrade", "home", ".mark", "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	var header struct{ Version int }
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load(captured graph.json): %v", err)
	}
	backlinks := s.Backlinks("mark://localhost:6310/fixture/adr/0001-decision.md")
	hub := slices.Contains(backlinks, "mark://localhost:6310/fixture/index.md")
	if current := header.Version == schemaVersion; hub != current || !current && s.NodeCount() != 0 {
		t.Fatalf("schema %d of %d: %d nodes, ADR backlinks %v", header.Version, schemaVersion, s.NodeCount(), backlinks)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("save after loading the capture: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("reload after save: %v", err)
	}
}
