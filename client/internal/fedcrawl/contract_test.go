package fedcrawl

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the graph-export contract golden file")

const contractGoldenPath = "testdata/graph-export-incluster.md"

// exportedLineRe matches the volatile timestamp header of an export.
var exportedLineRe = regexp.MustCompile(`(?m)^> Exported: .*$`)

// TestGraphExportContract pins the exact /graph.md body published for an
// in-cluster topology. The golden is the producer half of a cross-module
// contract; the broker's seed suite consumes the same file (-update regenerates).
func TestGraphExportContract(t *testing.T) {
	const host = "team-a.team-a.svc.cluster.local:6309"

	client := newMockClient()
	client.addList(host, "/", "- [index.md](index.md)\n- [a.md](a.md)\n- [b.md](b.md)\n")
	client.addDocWithMeta(host, "/index.md",
		"# Team A hub\n\n## Services\n\n- [Applications](/a.md)\n",
		"sha256-"+strings.Repeat("1", 64), map[string]string{"title": "Team A hub"})
	// No metadata title: the node's title must come from the H1 fallback
	// (the golden pins it, since the H1 text matches the old declared title).
	client.addDocWithMeta(host, "/a.md",
		"# Applications\n\n## Links\n\n- [B doc](/b.md)\n- [external](https://example.com/ext)\n- [dev](mark://localhost:6309/dev.md)\n",
		"sha256-"+strings.Repeat("2", 64), nil)
	client.addDocWithMeta(host, "/b.md",
		"# B doc\n",
		"sha256-"+strings.Repeat("3", 64), map[string]string{"title": "B doc", "rel-supersedes": "/a.md"})

	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://" + host}
	cfg.Crawl.MaxDocuments = 100
	cfg.Politeness.RequestDelay = 0

	crawler := NewCrawler(cfg, client, nil, nil)
	if _, err := crawler.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := exportedLineRe.ReplaceAllString(crawler.GraphExport(), "> Exported: GOLDEN")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(contractGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(contractGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(contractGoldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("agent /graph.md export drifted from the contract golden.\nIf intentional, regenerate with -update and re-run the broker suite (it consumes this file).\ngot:\n%s\nwant:\n%s", got, want)
	}

	// Contract facts pinned independently of bytes: rows key on the internal
	// address in identity form with the dial port removed (ADR 0005), and
	// non-mark targets never enter. The broker canonicalizes to match.
	if !strings.Contains(got, "mark://team-a.team-a.svc.cluster.local/a.md") {
		t.Error("export lost the internal address URL form")
	}
	if strings.Contains(got, host) {
		t.Errorf("export carries a dial address, not a node identity: %s", host)
	}
	if strings.Contains(got, "https://example.com/ext") || strings.Contains(got, "localhost") {
		t.Error("external or loopback target leaked into the federation export")
	}
}
