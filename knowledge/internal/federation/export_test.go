package federation

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graphstore"
)

// withBeta adds a second world, beta, holding one document.
func (h *harness) withBeta() {
	beta := newFakeWorld(h.t)
	beta.publish("/b.md", "# B\n\n[alpha](mark://alpha/a.md)\n")
	h.worlds["beta"] = beta
}

// awaitExport polls until the hub's export holds exactly these nodes, by URL
// to title.
func awaitExport(t *testing.T, hub generation.IO, want map[string]string) {
	t.Helper()
	poll(t, "export", func() error {
		head, err := hub.Fetch(context.Background(), graphstore.LegacyExportPath)
		if err != nil {
			return err
		}
		nodes, _, err := graphstore.ParseExportStrict(head.Body)
		if err != nil {
			return fmt.Errorf("%s: %w", head.Status, err)
		}
		got := map[string]string{}
		for i := range nodes {
			got[nodes[i].URL] = nodes[i].Title
		}
		if !maps.Equal(got, want) {
			return fmt.Errorf("nodes %v, want %v", got, want)
		}
		return nil
	})
}

var seededNodes = map[string]string{"mark://alpha/index.md": "Home", "mark://alpha/a.md": "A", "mark://alpha/docs/c.md": "C"}

func TestExporterWaitsForEveryWorldsCheckpoint(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/a.md", "# A\n")
	h.withBeta()
	h.hub.setFailing(graphstore.WorldManifestPath("beta"), true)
	h.start()
	h.await(versions(map[string]int{"/a.md": 1}))
	quiet()
	if n := h.hub.exportWrites(); n != 0 {
		t.Fatalf("export written %d times while beta had no checkpoint, want 0", n)
	}
	h.hub.setFailing(graphstore.WorldManifestPath("beta"), false)
	awaitExport(t, h.hub.io(), map[string]string{"mark://alpha/a.md": "A", "mark://beta/b.md": "B"})
	quiet()
	if n := h.hub.exportWrites(); n != 1 {
		t.Errorf("export written %d times, want once", n)
	}
}

func TestExporterWritesOnlyWhenTheGraphChanged(t *testing.T) {
	h, _ := seeded(t)
	awaitExport(t, h.hub.io(), seededNodes)
	quiet()
	before := h.hub.exportWrites()
	// A new version of the same document renders the same graph.
	h.alpha.publish("/a.md", "# A\n\n[home](/index.md)\n")
	h.await(versions(with(map[string]int{"/a.md": 2})))
	quiet()
	if n := h.hub.exportWrites(); n != before {
		t.Fatalf("export written %d times for an unchanged graph, want %d", n, before)
	}
	h.alpha.publish("/a.md", "# A renamed\n")
	renamed := maps.Clone(seededNodes)
	renamed["mark://alpha/a.md"] = "A renamed"
	awaitExport(t, h.hub.io(), renamed)
	if n := h.hub.exportWrites(); n != before+1 {
		t.Errorf("export written %d times, want %d", n, before+1)
	}
}

// After a conflict the export is rendered from the hub's checkpoints again,
// not from the ones this term loaded before another term wrote.
func TestExporterRendersFromTheHubAgainAfterAConflict(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/a.md", "# A\n")
	h.withBeta()
	h.start()
	awaitExport(t, h.hub.io(), map[string]string{"mark://alpha/a.md": "A", "mark://beta/b.md": "B"})

	// A newer term emptied beta's checkpoint and wrote the export.
	h.rewriteManifest("beta", func(m *graphstore.WorldManifest) { m.Shards = nil })
	if _, err := h.hub.publish(context.Background(), graphstore.LegacyExportPath, "# Document Graph\n", -1); err != nil {
		t.Fatal(err)
	}

	h.alpha.publish("/d.md", "# D\n")
	awaitExport(t, h.hub.io(), map[string]string{"mark://alpha/a.md": "A", "mark://alpha/d.md": "D"})
}

// A new term whose render matches the live export writes nothing.
func TestExporterOfANewTermLeavesAMatchingExportAlone(t *testing.T) {
	h, stop := seeded(t)
	awaitExport(t, h.hub.io(), seededNodes)
	stop()
	before := h.hub.exportWrites()
	h.start()
	quiet()
	if n := h.hub.exportWrites(); n != before {
		t.Errorf("export written %d times by a term that changed nothing, want %d", n, before)
	}
}

// rowCount checks a checkpoint holds n rows.
func rowCount(n int) func(graphstore.WorldManifest, map[string]graphstore.WorldSource) error {
	return func(_ graphstore.WorldManifest, rows map[string]graphstore.WorldSource) error {
		if len(rows) != n {
			return fmt.Errorf("%d rows, want %d", len(rows), n)
		}
		return nil
	}
}

// syncBuffer is a log sink the deriver's goroutines share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

func TestExporterSkipsAnExportPastTheBodyLimit(t *testing.T) {
	h := newHarness(t)
	// Rows of a thousand edges each: sixteen of them render past 1 MiB.
	big := func(i int) string {
		var body strings.Builder
		fmt.Fprintf(&body, "# Big %d\n\n", i)
		for j := range maxSourceEdges - 1 {
			fmt.Fprintf(&body, "[t](/targets/target-%04d-of-a-long-path.md)\n", j)
		}
		return body.String()
	}
	for i := range 16 {
		h.alpha.publish(fmt.Sprintf("/big-%d.md", i), big(i))
	}
	logs := &syncBuffer{}
	cfg := Config{Worlds: []string{"alpha"}, Source: h.worlds, Hub: h.hub.io(), Log: slog.New(slog.NewTextHandler(logs, nil))}
	runDeriver(t, testDeriver(cfg))
	h.await(rowCount(16))
	h.alpha.publish("/big-16.md", big(16))
	h.await(rowCount(17))
	quiet()
	const warning = "graph export exceeds the body limit"
	if n, logged := h.hub.exportWrites(), logs.count(warning); n != 0 || logged != 1 {
		t.Fatalf("an oversized export: written %d times, logged %d times, want 0 and once", n, logged)
	}
	for i := 1; i <= 16; i++ {
		h.alpha.archive(fmt.Sprintf("/big-%d.md", i))
	}
	awaitExport(t, h.hub.io(), map[string]string{"mark://alpha/big-0.md": "Big 0"})
}
