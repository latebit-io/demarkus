package federation

import (
	"os"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/graphstore"
)

// contractGoldenPath is the deriver's /graph.md for one world, minus its
// Exported line: the producer half of the contract the gateway's seed suite
// consumes. UPDATE_GOLDEN=1 regenerates it; rerun that suite after.
const contractGoldenPath = "testdata/graph-export-contract.md"

// TestGraphExportContract pins the exact /graph.md the deriver publishes for
// a world with a hub page, a typed relation, a repeated link and links the
// export must drop (external, loopback).
func TestGraphExportContract(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/index.md", "# Team A hub\n\n## Services\n\n- [Applications](/docs/a.md)\n")
	h.alpha.publish("/docs/a.md", "# Applications\n\n## Links\n\n- [B doc](b.md)\n- [B again](b.md)\n- [external](https://example.com/ext)\n- [dev](mark://localhost:6309/dev.md)\n")
	h.alpha.publishWith("/docs/b.md", "# B heading\n", map[string]string{"title": "B doc", "rel-supersedes": "a.md"})
	h.start()
	body := awaitExport(t, h.hub.io(), map[string]string{"mark://alpha/index.md": "Team A hub", "mark://alpha/docs/a.md": "Applications", "mark://alpha/docs/b.md": "B doc"})
	got := graphstore.ExportContent(body)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(contractGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(contractGoldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if got != string(want) {
		t.Errorf("/graph.md export drifted from the contract golden.\nIf intentional, regenerate with UPDATE_GOLDEN=1 and rerun the gateway seed suite (it consumes this file).\ngot:\n%s\nwant:\n%s", got, want)
	}
}
