package federation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/graphstore"
	"github.com/latebit-io/demarkus/protocol"
)

func TestGateCheckRefusesAGatedWorldIntoAnOpenHub(t *testing.T) {
	gated := map[string]bool{"private": true, "gated-hub": true}
	isGated := func(world string) bool { return gated[world] }
	open := GateCheck("hub", isGated)
	if err := open("private"); !errors.Is(err, ErrGatedIntoOpenHub) {
		t.Errorf("gated world, open hub: err = %v, want ErrGatedIntoOpenHub", err)
	}
	if err := open("public"); err != nil {
		t.Errorf("open world, open hub: %v", err)
	}
	// A gated hub is read by the readers the gated world admits.
	closed := GateCheck("gated-hub", isGated)
	for _, world := range []string{"private", "public"} {
		if err := closed(world); err != nil {
			t.Errorf("%s into a gated hub: %v", world, err)
		}
	}
}

// The deriver reads in process, past a world's gate: a gated world must not
// be read or checkpointed into a hub anonymous readers can fetch.
func TestDeriverLeavesARefusedWorldUnreadAndUncheckpointed(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/index.md", "# Private\n\n[a](a.md)\n")
	h.alpha.publish("/a.md", "# A\n")
	var refuse atomic.Bool
	refuse.Store(true)
	stop := h.startGated(&refuse)
	quiet()
	if reads := h.alpha.reads("/index.md"); reads != 0 {
		t.Fatalf("a refused world was read %d times", reads)
	}
	if _, _, err := h.checkpoint(); err == nil {
		t.Fatal("a refused world has a checkpoint in the hub")
	}

	// The gate lifts (the hub was gated): the world is derived.
	refuse.Store(false)
	h.await(versions(map[string]int{"/index.md": 1, "/a.md": 1}))

	// And closes again under the running loop: what the hub held is
	// withdrawn, and a later change is neither read nor checkpointed.
	refuse.Store(true)
	h.await(withdrawn)
	h.alpha.publish("/later.md", "# Later\n")
	quiet()
	if reads := h.alpha.reads("/later.md"); reads != 0 {
		t.Fatalf("a refused world's change was read %d times", reads)
	}
	if _, rows, err := h.checkpoint(); err != nil || len(rows) != 0 {
		t.Fatalf("checkpoint after the world was refused: rows %v, %v", rows, err)
	}
	stop()
}

// withdrawn is a checkpoint emptied on purpose: no rows, no pins, incomplete
// so a world derived again rebuilds.
func withdrawn(m graphstore.WorldManifest, rows map[string]graphstore.WorldSource) error {
	if len(rows) != 0 || len(m.Shards) != 0 || m.Complete {
		return fmt.Errorf("complete %t, %d shards, rows %v; want a withdrawn checkpoint", m.Complete, len(m.Shards), rows)
	}
	return nil
}

// A world gated while nothing in it changes still leaves the open hub: its
// shards and manifest emptied, the export rendered without it, each written
// so the versions that held its rows are pruned.
func TestGatedWorldIsWithdrawnFromAnOpenHub(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/index.md", "# Private\n\n[a](a.md)\n")
	h.alpha.publish("/a.md", "# A\n")
	var refuse atomic.Bool
	stop := h.startGated(&refuse)
	before := h.await(versions(map[string]int{"/index.md": 1, "/a.md": 1}))
	poll(t, "export of alpha", func() error {
		if !strings.Contains(h.hub.live(graphstore.GraphExportPath), "a.md") {
			return errors.New("the export does not list the world yet")
		}
		return nil
	})

	// A shard no manifest pins: written before its manifest, or split away.
	orphan := graphstore.WorldShardPath("alpha", "ab")
	if _, err := h.hub.io().Publish(context.Background(), orphan, "# rows no manifest pins\n", -1); err != nil {
		t.Fatal(err)
	}

	// And a document that is no shard: left alone, and no obstacle.
	stray := graphstore.WorldGraphRoot + "/alpha/README.md"
	if _, err := h.hub.io().Publish(context.Background(), stray, "# Not a shard\n", -1); err != nil {
		t.Fatal(err)
	}

	refuse.Store(true)
	h.await(withdrawn)
	poll(t, "export without alpha", func() error {
		if strings.Contains(h.hub.live(graphstore.GraphExportPath), "a.md") {
			return errors.New("the export still lists the withdrawn world")
		}
		return nil
	})
	stop()

	for _, ref := range before.Shards {
		empty, err := graphstore.BuildWorldShard("alpha", ref.Prefix, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := h.hub.live(ref.Path); got != empty.Body {
			t.Errorf("shard %s after the withdrawal is not empty:\n%s", ref.Path, got)
		}
	}
	if strings.Contains(h.hub.live(orphan), "rows") {
		t.Errorf("the unpinned shard %s kept its rows", orphan)
	}
	if got := h.hub.live(stray); got != "# Not a shard\n" {
		t.Errorf("a document that is no shard was rewritten: %q", got)
	}
	// Every version from before the withdrawal held the rows: none is left.
	held := map[string]int{graphstore.WorldManifestPath("alpha"): 1, graphstore.GraphExportPath: 1, orphan: 1}
	for _, ref := range before.Shards {
		held[ref.Path] = ref.Version
	}
	for path, version := range held {
		if h.hub.live(protocol.VersionPath(path, version)) != "" {
			t.Errorf("%s v%d, written before the withdrawal, is still readable", path, version)
		}
	}
}

// The lister names the documents directly in a directory, and reads a
// directory that does not exist as empty.
func TestHubListerListsOneDirectory(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/index.md", "# Home\n")
	h.alpha.publish("/docs/c.md", "# C\n")
	list := HubLister(h.worlds, "alpha")
	got, err := list(context.Background(), "/")
	if err != nil || !slices.Equal(got, []string{"/index.md"}) {
		t.Errorf("list / = %v, %v; want /index.md alone", got, err)
	}
	if got, err := list(context.Background(), "/nowhere"); err != nil || len(got) != 0 {
		t.Errorf("list of a missing directory = %v, %v; want empty", got, err)
	}
}

// A term that finds the withdrawn manifest writes nothing more but still
// asks for the export's prune: the term that wrote it may have ended first.
func TestWithdrawAsksForTheExportPruneOverAMarkerManifest(t *testing.T) {
	h := newHarness(t)
	d := testDeriver(h.config())
	ctx := context.Background()
	body, err := graphstore.BuildWorldManifest(graphstore.WorldManifest{World: "alpha", Cursor: protocol.Cursor{Epoch: withdrawnEpoch}, PrefixLength: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.hub.io().Publish(ctx, graphstore.WorldManifestPath("alpha"), body, -1); err != nil {
		t.Fatal(err)
	}
	mark := h.hub.mark()
	if err := d.withdraw(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := h.hub.written(mark); len(got) != 0 {
		t.Errorf("a landed withdrawal was written again at %v", got)
	}
	d.export.mu.Lock()
	defer d.export.mu.Unlock()
	if d.export.withdrawals != 1 {
		t.Errorf("withdrawals = %d, want the export asked to prune once", d.export.withdrawals)
	}
}

// A render built before a withdrawn world's checkpoint was loaded must not
// answer for the withdrawal: it still holds the rows, and the one that drops
// them would then keep it as an older version.
func TestStaleRenderDoesNotClaimAWithdrawal(t *testing.T) {
	h, stop := seeded(t)
	awaitExport(t, h.hub.io(), seededNodes)
	stop()
	ctx := context.Background()
	e := testDeriver(h.config()).export
	e.pendAll()
	if err := e.export(ctx); err != nil {
		t.Fatal(err)
	}
	// The withdrawal arrives after the loads: this render predates it.
	e.withdrawn("alpha")
	e.owed = true
	if err := e.write(ctx); err != nil {
		t.Fatal(err)
	}
	if e.pruned != 0 {
		t.Fatal("a render from before the withdrawal was counted as its prune")
	}
	// The next pass loads the withdrawn world, and its render prunes.
	if err := e.export(ctx); err != nil {
		t.Fatal(err)
	}
	if e.pruned != 1 {
		t.Errorf("pruned = %d after the withdrawn world was loaded, want 1", e.pruned)
	}
}
