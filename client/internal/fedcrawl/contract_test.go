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

// TestGraphExportContract pins the exact /graph.md body the agent publishes
// when crawling an in-cluster topology (worlds seeded by service DNS). The
// golden file is the producer half of a cross-module contract: the broker's
// seed path consumes the same file in its own suite, so a drift in either
// the export shape or the URL forms fails a build instead of shipping.
// Regenerate with: go test ./internal/fedcrawl -run TestGraphExportContract -update
func TestGraphExportContract(t *testing.T) {
	const host = "team-a.team-a.svc.cluster.local:6309"

	client := newMockClient()
	client.addList(host, "/", "- [index.md](index.md)\n- [a.md](a.md)\n- [b.md](b.md)\n")
	client.addDocWithMeta(host, "/index.md",
		"# Team A hub\n\n## Services\n\n- [Applications](/a.md)\n",
		"sha256-"+strings.Repeat("1", 64), map[string]string{"title": "Team A hub"})
	client.addDocWithMeta(host, "/a.md",
		"# Applications\n\n## Links\n\n- [B doc](/b.md)\n- [external](https://example.com/ext)\n- [dev](mark://localhost:6309/dev.md)\n",
		"sha256-"+strings.Repeat("2", 64), map[string]string{"title": "Applications"})
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

	// Contract facts the consumer relies on, pinned independently of bytes:
	// rows key on the internal dial address, and non-mark targets never enter.
	if !strings.Contains(got, "mark://"+host+"/a.md") {
		t.Error("export lost the internal dial-address URL form")
	}
	if strings.Contains(got, "https://example.com/ext") || strings.Contains(got, "localhost") {
		t.Error("external or loopback target leaked into the federation export")
	}
}
